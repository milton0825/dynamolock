/*
Copyright 2021 U. Cirello (cirello.io and github.com/cirello-io)

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamolock

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	attrData                = "data"
	attrOwnerName           = "ownerName"
	attrLeaseDuration       = "leaseDuration"
	attrRecordVersionNumber = "recordVersionNumber"
	attrIsReleased          = "isReleased"

	defaultBuffer = 1 * time.Second
)

var (
	dataAttr          = expression.Name(attrData)
	ownerNameAttr     = expression.Name(attrOwnerName)
	leaseDurationAttr = expression.Name(attrLeaseDuration)
	rvnAttr           = expression.Name(attrRecordVersionNumber)
	isReleasedAttr    = expression.Name(attrIsReleased)
)

var isReleasedAttrVal = expression.Value("1")

type commonClient struct {
	dynamoDB DynamoDBClient

	tableName        string
	partitionKeyName string

	leaseDuration               time.Duration
	heartbeatPeriod             time.Duration
	ownerName                   string
	locks                       sync.Map
	sessionMonitorCancellations sync.Map

	logger ContextLeveledLogger

	stopHeartbeat context.CancelFunc

	mu        sync.RWMutex
	closeOnce sync.Once
	closed    bool
}

const (
	defaultLeaseDuration   = 20 * time.Second
	defaultHeartbeatPeriod = 5 * time.Second
)

func newCommon(dynamoDB DynamoDBClient, tableName, partitionKeyName string, opts ...ClientOption) (*commonClient, error) {
	c := &commonClient{
		dynamoDB:         dynamoDB,
		tableName:        tableName,
		partitionKeyName: partitionKeyName,
		leaseDuration:    defaultLeaseDuration,
		heartbeatPeriod:  defaultHeartbeatPeriod,
		ownerName:        randString(32),
		logger:           &plainLogger{logger: log.New(ioutil.Discard, "", 0)},
		stopHeartbeat:    func() {},
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.leaseDuration < 2*c.heartbeatPeriod {
		return nil, errors.New("heartbeat period must be no more than half the length of the Lease Duration, " +
			"or locks might expire due to the heartbeat thread taking too long to update them (recommendation is to make it much greater, for example " +
			"4+ times greater)")
	}

	if c.heartbeatPeriod > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		c.stopHeartbeat = cancel
		go c.heartbeat(ctx)
	}
	return c, nil
}

// ClientOption reconfigure the lock client creation.
type ClientOption func(*commonClient)

// WithOwnerName changes the owner linked to the client, and by consequence to
// locks.
func WithOwnerName(s string) ClientOption {
	return func(c *commonClient) { c.ownerName = s }
}

// WithLeaseDuration defines how long should the lease be held.
func WithLeaseDuration(d time.Duration) ClientOption {
	return func(c *commonClient) { c.leaseDuration = d }
}

// WithHeartbeatPeriod defines the frequency of the heartbeats. Set to zero to
// disable it. Heartbeats should have no more than half of the duration of the
// lease.
func WithHeartbeatPeriod(d time.Duration) ClientOption {
	return func(c *commonClient) { c.heartbeatPeriod = d }
}

// DisableHeartbeat disables automatic hearbeats. Use SendHeartbeat to freshen
// up the lock.
func DisableHeartbeat() ClientOption {
	return WithHeartbeatPeriod(0)
}

// WithLogger injects a logger into the client, so its internals can be
// recorded.
func WithLogger(l Logger) ClientOption {
	return func(c *commonClient) { c.logger = &plainLogger{l} }
}

// WithLeveledLogger injects a logger into the client, so its internals can be
// recorded.
func WithLeveledLogger(l LeveledLogger) ClientOption {
	return func(c *commonClient) { c.logger = &contextLoggerAdapter{l} }
}

// WithContextLeveledLogger injects a logger into the client, so its internals can be
// recorded.
func WithContextLeveledLogger(l ContextLeveledLogger) ClientOption {
	return func(c *commonClient) { c.logger = l }
}

// AcquireLockOption allows to change how the lock is actually held by the
// client.
type AcquireLockOption func(*acquireLockOptions)

// WithData stores the content into the lock itself.
func WithData(b []byte) AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.data = b
	}
}

// ReplaceData will force the new content to be stored in the key.
func ReplaceData() AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.replaceData = true
	}
}

// FailIfLocked will not retry to acquire the lock, instead returning.
func FailIfLocked() AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.failIfLocked = true
	}
}

// WithDeleteLockOnRelease defines whether or not the lock should be deleted
// when Close() is called on the resulting LockItem will force the new content
// to be stored in the key.
func WithDeleteLockOnRelease() AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.deleteLockOnRelease = true
	}
}

// WithRefreshPeriod defines how long to wait before trying to get the lock
// again (if set to 10 seconds, for example, it would attempt to do so every 10
// seconds).
func WithRefreshPeriod(d time.Duration) AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.refreshPeriod = d
	}
}

// WithAdditionalTimeToWaitForLock defines how long to wait in addition to the
// lease duration (if set to 10 minutes, this will try to acquire a lock for at
// least 10 minutes before giving up and returning an error).
func WithAdditionalTimeToWaitForLock(d time.Duration) AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.additionalTimeToWaitForLock = d
	}
}

// WithAdditionalAttributes stores some additional attributes with each lock.
// This can be used to add any arbitrary parameters to each lock row.
func WithAdditionalAttributes(attr map[string]types.AttributeValue) AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.additionalAttributes = attr
	}
}

// WithSessionMonitor registers a callback that is triggered if the lock is
// about to expire.
//
// The purpose of this construct is to provide two abilities: provide
// the ability to determine if the lock is about to expire, and run a
// user-provided callback when the lock is about to expire. The advantage
// this provides is notification that your lock is about to expire before it
// is actually expired, and in case of leader election will help in
// preventing that there are no two leaders present simultaneously.
//
// If due to any reason heartbeating is unsuccessful for a configurable
// period of time, your lock enters into a phase known as "danger zone." It
// is during this "danger zone" that the callback will be run.
//
// Bear in mind that the callback may be null. In this
// case, no callback will be run upon the lock entering the "danger zone";
// yet, one can still make use of the Lock.IsAlmostExpired() call.
// Furthermore, non-null callbacks can only ever be executed once in a
// lock's lifetime. Independent of whether or not a callback is run, the
// client will attempt to heartbeat the lock until the lock is released or
// obtained by someone else.
//
// Consider an example which uses this mechanism for leader election. One
// way to make use of this SessionMonitor is to register a callback that
// kills the instance in case the leader's lock enters the danger zone:
func WithSessionMonitor(safeTime time.Duration, callback func()) AcquireLockOption {
	return func(opt *acquireLockOptions) {
		opt.sessionMonitor = &sessionMonitor{
			safeTime: safeTime,
			callback: callback,
		}
	}
}

func (c *commonClient) acquireLock(ctx context.Context, partitionKey string, opts ...AcquireLockOption) (*Lock, error) {
	if c.isClosed() {
		return nil, ErrClientClosed
	}
	opt := &acquireLockOptions{
		partitionKey: partitionKey,
	}
	for _, o := range opts {
		o(opt)
	}

	// Hold the read lock when acquiring locks. This prevents us from
	// acquiring a lock while the Client is being closed as we hold the
	// write lock during close.
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, ErrClientClosed
	}

	attrs := opt.additionalAttributes
	contains := func(ks ...string) bool {
		for _, k := range ks {
			if _, ok := attrs[k]; ok {
				return true
			}
		}
		return false
	}

	if contains(c.partitionKeyName, attrOwnerName, attrLeaseDuration,
		attrRecordVersionNumber, attrData) {
		return nil, fmt.Errorf("additional attribute cannot be one of the following types: %s, %s, %s, %s, %s",
			c.partitionKeyName, attrOwnerName, attrLeaseDuration, attrRecordVersionNumber, attrData)
	}

	getLockOptions := getLockOptions{
		partitionKey:         opt.partitionKey,
		deleteLockOnRelease:  opt.deleteLockOnRelease,
		sessionMonitor:       opt.sessionMonitor,
		start:                time.Now(),
		replaceData:          opt.replaceData,
		data:                 opt.data,
		additionalAttributes: attrs,
		failIfLocked:         opt.failIfLocked,
	}

	getLockOptions.millisecondsToWait = defaultBuffer
	if opt.additionalTimeToWaitForLock > 0 {
		getLockOptions.millisecondsToWait = opt.additionalTimeToWaitForLock
	}

	getLockOptions.refreshPeriodDuration = defaultBuffer
	if opt.refreshPeriod > 0 {
		getLockOptions.refreshPeriodDuration = opt.refreshPeriod
	}

	for {
		l, err := c.storeLock(ctx, &getLockOptions)
		if err != nil {
			return nil, err
		} else if l != nil {
			return l, nil
		}
		c.logger.Info(ctx, "Sleeping for a refresh period of ", getLockOptions.refreshPeriodDuration)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(getLockOptions.refreshPeriodDuration):
		}
	}
}

func (c *commonClient) storeLock(ctx context.Context, getLockOptions *getLockOptions) (*Lock, error) {
	c.logger.Info(ctx, "Call GetItem to see if the lock for ",
		c.partitionKeyName, " =", getLockOptions.partitionKey, " exists in the table")
	existingLock, err := c.getLockFromDynamoDB(ctx, *getLockOptions)
	if err != nil {
		return nil, err
	}

	var newLockData []byte
	if getLockOptions.replaceData {
		newLockData = getLockOptions.data
	} else if existingLock != nil {
		newLockData = existingLock.data
	}

	if newLockData == nil {
		// If there is no existing data, we write the input data to the lock.
		newLockData = getLockOptions.data
	}

	mergedAdditionalAttributes := make(map[string]types.AttributeValue)
	for k, v := range existingLock.AdditionalAttributes() {
		mergedAdditionalAttributes[k] = v
	}
	for k, v := range getLockOptions.additionalAttributes {
		mergedAdditionalAttributes[k] = v
	}
	getLockOptions.additionalAttributes = mergedAdditionalAttributes

	item := make(map[string]types.AttributeValue)
	for k, v := range getLockOptions.additionalAttributes {
		item[k] = v
	}
	item[c.partitionKeyName] = stringAttrValue(getLockOptions.partitionKey)
	item[attrOwnerName] = stringAttrValue(c.ownerName)
	item[attrLeaseDuration] = stringAttrValue(c.leaseDuration.String())

	recordVersionNumber := c.generateRecordVersionNumber()
	item[attrRecordVersionNumber] = stringAttrValue(recordVersionNumber)

	if newLockData != nil {
		item[attrData] = bytesAttrValue(newLockData)
	}

	//if the existing lock does not exist or exists and is released
	if existingLock == nil || existingLock.isReleased {
		l, err := c.upsertAndMonitorNewOrReleasedLock(
			ctx,
			getLockOptions.additionalAttributes,
			getLockOptions.partitionKey,
			getLockOptions.deleteLockOnRelease,
			newLockData,
			item,
			recordVersionNumber,
			getLockOptions.sessionMonitor)
		if err != nil {
			var errNotGranted *LockNotGrantedError
			if errors.As(err, &errNotGranted) {
				return nil, nil
			}
		}
		return l, err
	}

	// we know that we didnt enter the if block above because it returns at the end.
	// we also know that the existingLock.isPresent() is true
	if getLockOptions.lockTryingToBeAcquired == nil {
		//this branch of logic only happens once, in the first iteration of the while loop
		//lockTryingToBeAcquired only ever gets set to non-null values after this point.
		//so it is impossible to get in this
		/*
		 * Someone else has the lock, and they have the lock for LEASE_DURATION time. At this point, we need
		 * to wait at least LEASE_DURATION milliseconds before we can try to acquire the lock.
		 */

		// If the user has set `FailIfLocked` option, exit after the first attempt to acquire the lock.
		if getLockOptions.failIfLocked {
			return nil, &LockNotGrantedError{msg: "Didn't acquire lock because it is locked and request is configured not to retry."}
		}

		getLockOptions.lockTryingToBeAcquired = existingLock
		if !getLockOptions.alreadySleptOnceForOneLeasePeriod {
			getLockOptions.alreadySleptOnceForOneLeasePeriod = true
			getLockOptions.millisecondsToWait += existingLock.leaseDuration
		}
	} else if getLockOptions.lockTryingToBeAcquired.recordVersionNumber == existingLock.recordVersionNumber && getLockOptions.lockTryingToBeAcquired.isExpired() {
		/* If the version numbers match, then we can acquire the lock, assuming it has already expired */
		l, err := c.upsertAndMonitorExpiredLock(
			ctx,
			getLockOptions.additionalAttributes,
			getLockOptions.partitionKey,
			getLockOptions.deleteLockOnRelease,
			existingLock, newLockData, item,
			recordVersionNumber,
			getLockOptions.sessionMonitor)
		if err != nil {
			var errNotGranted *LockNotGrantedError
			if errors.As(err, &errNotGranted) {
				return nil, nil
			}
		}
		return l, err
	} else if getLockOptions.lockTryingToBeAcquired.recordVersionNumber != existingLock.recordVersionNumber {
		/*
		 * If the version number changed since we last queried the lock, then we need to update
		 * lockTryingToBeAcquired as the lock has been refreshed since we last checked
		 */
		getLockOptions.lockTryingToBeAcquired = existingLock
	}

	if t := time.Since(getLockOptions.start); t > getLockOptions.millisecondsToWait {
		return nil, &LockNotGrantedError{
			msg:   "Didn't acquire lock after sleeping",
			cause: &TimeoutError{Age: t},
		}
	}
	return nil, nil
}

func (c *commonClient) upsertAndMonitorExpiredLock(
	ctx context.Context,
	additionalAttributes map[string]types.AttributeValue,
	partitionKey string,
	deleteLockOnRelease bool,
	existingLock *Lock,
	newLockData []byte,
	item map[string]types.AttributeValue,
	recordVersionNumber string,
	sessionMonitor *sessionMonitor,
) (*Lock, error) {
	cond := expression.And(
		expression.AttributeExists(expression.Name(c.partitionKeyName)),
		expression.Equal(rvnAttr, expression.Value(existingLock.recordVersionNumber)),
	)
	putItemExpr, _ := expression.NewBuilder().WithCondition(cond).Build()
	putItemRequest := &dynamodb.PutItemInput{
		Item:                      item,
		TableName:                 aws.String(c.tableName),
		ConditionExpression:       putItemExpr.Condition(),
		ExpressionAttributeNames:  putItemExpr.Names(),
		ExpressionAttributeValues: putItemExpr.Values(),
	}

	c.logger.Info(ctx, "Acquiring an existing lock whose revisionVersionNumber did not change for ",
		c.partitionKeyName, " partitionKey=", partitionKey)
	return c.putLockItemAndStartSessionMonitor(
		ctx, additionalAttributes, partitionKey, deleteLockOnRelease, newLockData,
		recordVersionNumber, sessionMonitor, putItemRequest)
}

func (c *commonClient) upsertAndMonitorNewOrReleasedLock(
	ctx context.Context,
	additionalAttributes map[string]types.AttributeValue,
	partitionKey string,
	deleteLockOnRelease bool,
	newLockData []byte,
	item map[string]types.AttributeValue,
	recordVersionNumber string,
	sessionMonitor *sessionMonitor,
) (*Lock, error) {
	cond := expression.Or(
		expression.AttributeNotExists(expression.Name(c.partitionKeyName)),
		expression.And(
			expression.AttributeExists(expression.Name(c.partitionKeyName)),
			expression.Equal(isReleasedAttr, isReleasedAttrVal),
		),
	)
	putItemExpr, _ := expression.NewBuilder().WithCondition(cond).Build()

	req := &dynamodb.PutItemInput{
		Item:                      item,
		TableName:                 aws.String(c.tableName),
		ConditionExpression:       putItemExpr.Condition(),
		ExpressionAttributeNames:  putItemExpr.Names(),
		ExpressionAttributeValues: putItemExpr.Values(),
	}

	// No one has the lock, go ahead and acquire it. The person storing the
	// lock into DynamoDB should err on the side of thinking the lock will
	// expire sooner than it actually will, so they start counting towards
	// its expiration before the Put succeeds
	c.logger.Info(ctx, "Acquiring a new lock or an existing yet released lock on ", c.partitionKeyName, "=", partitionKey)
	return c.putLockItemAndStartSessionMonitor(ctx, additionalAttributes, partitionKey,
		deleteLockOnRelease, newLockData,
		recordVersionNumber, sessionMonitor, req)
}

func (c *commonClient) putLockItemAndStartSessionMonitor(
	ctx context.Context,
	additionalAttributes map[string]types.AttributeValue,
	partitionKey string,
	deleteLockOnRelease bool,
	newLockData []byte,
	recordVersionNumber string,
	sessionMonitor *sessionMonitor,
	putItemRequest *dynamodb.PutItemInput) (*Lock, error) {

	lastUpdatedTime := time.Now()

	_, err := c.dynamoDB.PutItem(ctx, putItemRequest)
	if err != nil {
		return nil, parseDynamoDBError(err, "cannot store lock item: lock already acquired by other client")
	}

	releaseLock := func(ctx context.Context, lock *Lock) error {
		_, err := c.ReleaseLock(ctx, lock)
		return err
	}

	lockItem := &Lock{
		releaseLock:          releaseLock,
		partitionKey:         partitionKey,
		data:                 newLockData,
		deleteLockOnRelease:  deleteLockOnRelease,
		ownerName:            c.ownerName,
		leaseDuration:        c.leaseDuration,
		lookupTime:           lastUpdatedTime,
		recordVersionNumber:  recordVersionNumber,
		additionalAttributes: additionalAttributes,
		sessionMonitor:       sessionMonitor,
	}

	c.locks.Store(lockItem.uniqueIdentifier(), lockItem)
	c.tryAddSessionMonitor(lockItem.uniqueIdentifier(), lockItem)
	return lockItem, nil
}

func (c *commonClient) getLockFromDynamoDB(ctx context.Context, opt getLockOptions) (*Lock, error) {
	res, err := c.readFromDynamoDB(ctx, opt.partitionKey)
	if err != nil {
		return nil, err
	}

	item := res.Item
	if item == nil {
		return nil, nil
	}

	return c.createLockItem(opt, item)
}

func (c *commonClient) readFromDynamoDB(ctx context.Context, partitionKey string) (*dynamodb.GetItemOutput, error) {
	dynamoDBKey := map[string]types.AttributeValue{
		c.partitionKeyName: stringAttrValue(partitionKey),
	}
	return c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		ConsistentRead: aws.Bool(true),
		TableName:      aws.String(c.tableName),
		Key:            dynamoDBKey,
	})
}

func (c *commonClient) createLockItem(opt getLockOptions, item map[string]types.AttributeValue) (*Lock, error) {
	var data []byte

	if r, ok := item[attrData]; ok {
		data = readBytesAttr(r)
		delete(item, attrData)
	}

	ownerName := readStringAttr(item[attrOwnerName])
	delete(item, attrOwnerName)

	leaseDuration := readStringAttr(item[attrLeaseDuration])
	delete(item, attrLeaseDuration)

	recordVersionNumber := readStringAttr(item[attrRecordVersionNumber])
	delete(item, attrRecordVersionNumber)

	_, isReleased := item[attrIsReleased]
	delete(item, attrIsReleased)
	delete(item, c.partitionKeyName)

	// The person retrieving the lock in DynamoDB should err on the side of
	// not expiring the lock, so they don't start counting until after the
	// call to DynamoDB succeeds
	lookupTime := time.Now()

	var parsedLeaseDuration time.Duration
	if leaseDuration != "" {
		var err error
		parsedLeaseDuration, err = time.ParseDuration(leaseDuration)
		if err != nil {
			return nil, fmt.Errorf("cannot parse lease duration: %s", err)
		}
	}

	releaseLock := func(ctx context.Context, lock *Lock) error {
		_, err := c.ReleaseLock(ctx, lock)
		return err
	}

	lockItem := &Lock{
		releaseLock:          releaseLock,
		partitionKey:         opt.partitionKey,
		data:                 data,
		deleteLockOnRelease:  opt.deleteLockOnRelease,
		ownerName:            ownerName,
		leaseDuration:        parsedLeaseDuration,
		lookupTime:           lookupTime,
		recordVersionNumber:  recordVersionNumber,
		isReleased:           isReleased,
		additionalAttributes: item,
	}
	return lockItem, nil
}

func (c *commonClient) generateRecordVersionNumber() string {
	// TODO: improve me
	return randString(32)
}

var letterRunes = []rune("1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randString(n int) string {
	b := make([]rune, n)
	for i := range b {
		// ignoring error as the only possible error is for io.ReadFull
		r, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letterRunes))))
		b[i] = letterRunes[r.Int64()]
	}
	return string(b)
}

func (c *commonClient) heartbeat(ctx context.Context) {
	c.logger.Info(ctx, "starting heartbeats")
	tick := time.NewTicker(c.heartbeatPeriod)
	defer tick.Stop()
	for range tick.C {
		c.locks.Range(func(_ interface{}, value interface{}) bool {
			lockItem := value.(*Lock)
			if err := c.SendHeartbeat(ctx, lockItem); err != nil {
				c.logger.Error(ctx, "error sending heartbeat to", lockItem.partitionKey, ":", err)
			}
			return true
		})
		if ctx.Err() != nil {
			c.logger.Error(ctx, "client closed, stopping heartbeat")
			return
		}
	}
}

type createTableSchema func() ([]types.KeySchemaElement, []types.AttributeDefinition)

// CreateTable prepares a DynamoDB table with the right schema for it
// to be used by this locking library. The table should be set up in advance,
// because it takes a few minutes for DynamoDB to provision a new instance.
// Also, if the table already exists, it will return an error. The given context
// is passed down to the underlying dynamoDB call.
func (c *commonClient) CreateTable(ctx context.Context, cts createTableSchema,
	opts ...CreateTableOption) (*dynamodb.CreateTableOutput, error) {
	if c.isClosed() {
		return nil, ErrClientClosed
	}
	createTableOptions := &createDynamoDBTableOptions{
		billingMode: "PAY_PER_REQUEST",
	}
	for _, opt := range opts {
		opt(createTableOptions)
	}
	return c.createTable(ctx, cts, createTableOptions)
}

// CreateTableOption is an options type for the CreateTable method in the lock
// client. This allows the user to create a DynamoDB table that is lock
// client-compatible and specify optional parameters such as the desired
// throughput and whether or not to use a sort key.
type CreateTableOption func(*createDynamoDBTableOptions)

// WithTags changes the tags of the table. If not specified, the table will have empty tags.
func WithTags(tags []types.Tag) CreateTableOption {
	return func(opt *createDynamoDBTableOptions) {
		opt.tags = tags
	}
}

// WithProvisionedThroughput changes the billing mode of DynamoDB
// and tells DynamoDB to operate in a provisioned throughput mode instead of pay-per-request
func WithProvisionedThroughput(provisionedThroughput *types.ProvisionedThroughput) CreateTableOption {
	return func(opt *createDynamoDBTableOptions) {
		opt.billingMode = types.BillingModeProvisioned
		opt.provisionedThroughput = provisionedThroughput
	}
}

func (c *commonClient) createTable(ctx context.Context, cts createTableSchema, opt *createDynamoDBTableOptions) (*dynamodb.CreateTableOutput, error) {
	keySchema, attributeDefinitions := cts()

	createTableInput := &dynamodb.CreateTableInput{
		TableName:            aws.String(c.tableName),
		KeySchema:            keySchema,
		BillingMode:          opt.billingMode,
		AttributeDefinitions: attributeDefinitions,
	}

	if opt.provisionedThroughput != nil {
		createTableInput.ProvisionedThroughput = opt.provisionedThroughput
	}

	if opt.tags != nil {
		createTableInput.Tags = opt.tags
	}

	return c.dynamoDB.CreateTable(ctx, createTableInput)
}

// ReleaseLock releases the given lock if the current user still has it,
// returning true if the lock was successfully released, and false if someone
// else already stole the lock or a problem happened. Deletes the lock item if
// it is released and deleteLockItemOnClose is set.
func (c *commonClient) ReleaseLock(ctx context.Context, lockItem *Lock, opts ...ReleaseLockOption) (bool, error) {
	if c.isClosed() {
		return false, ErrClientClosed
	}
	err := c.releaseLock(ctx, lockItem, opts...)
	return err == nil, err
}

// WithDeleteLock defines whether or not to delete the lock when releasing it.
// If set to false, the lock row will continue to be in DynamoDB, but it will be
// marked as released.
func WithDeleteLock(deleteLock bool) ReleaseLockOption {
	return func(opt *releaseLockOptions) {
		opt.deleteLock = deleteLock
	}
}

// WithDataAfterRelease is the new data to persist to the lock (only used if
// deleteLock=false.) If the data is null, then the lock client will keep the
// data as-is and not change it.
func WithDataAfterRelease(data []byte) ReleaseLockOption {
	return func(opt *releaseLockOptions) {
		opt.data = data
	}
}

// ReleaseLockOption provides options for releasing a lock when calling the
// releaseLock() method. This class contains the options that may be configured
// during the act of releasing a lock.
type ReleaseLockOption func(*releaseLockOptions)

func ownershipLockCondition(partitionKeyName, recordVersionNumber, ownerName string) expression.ConditionBuilder {
	cond := expression.And(
		expression.And(
			expression.AttributeExists(expression.Name(partitionKeyName)),
			expression.Equal(rvnAttr, expression.Value(recordVersionNumber)),
		),
		expression.Equal(ownerNameAttr, expression.Value(ownerName)),
	)
	return cond
}

func (c *commonClient) releaseLock(ctx context.Context, lockItem *Lock, opts ...ReleaseLockOption) error {
	options := &releaseLockOptions{
		lockItem: lockItem,
	}
	if lockItem != nil {
		options.deleteLock = lockItem.deleteLockOnRelease
	}
	for _, opt := range opts {
		opt(options)
	}

	if lockItem == nil {
		return ErrCannotReleaseNullLock
	}
	deleteLock := options.deleteLock
	data := options.data

	if lockItem.ownerName != c.ownerName {
		return ErrOwnerMismatched
	}

	lockItem.semaphore.Lock()
	defer lockItem.semaphore.Unlock()

	lockItem.isReleased = true
	c.locks.Delete(lockItem.uniqueIdentifier())

	key := c.getItemKeys(lockItem)
	ownershipLockCond := ownershipLockCondition(c.partitionKeyName, lockItem.recordVersionNumber, lockItem.ownerName)
	if deleteLock {
		err := c.deleteLock(ctx, ownershipLockCond, key)
		if err != nil {
			return err
		}
	} else {
		err := c.updateLock(ctx, data, ownershipLockCond, key)
		if err != nil {
			return err
		}
	}
	c.removeKillSessionMonitor(lockItem.uniqueIdentifier())
	return nil
}

func (c *commonClient) deleteLock(ctx context.Context, ownershipLockCond expression.ConditionBuilder, key map[string]types.AttributeValue) error {
	delExpr, _ := expression.NewBuilder().WithCondition(ownershipLockCond).Build()
	deleteItemRequest := &dynamodb.DeleteItemInput{
		TableName:                 aws.String(c.tableName),
		Key:                       key,
		ConditionExpression:       delExpr.Condition(),
		ExpressionAttributeNames:  delExpr.Names(),
		ExpressionAttributeValues: delExpr.Values(),
	}
	_, err := c.dynamoDB.DeleteItem(ctx, deleteItemRequest)
	if err != nil {
		return err
	}
	return nil
}

func (c *commonClient) updateLock(ctx context.Context, data []byte, ownershipLockCond expression.ConditionBuilder, key map[string]types.AttributeValue) error {
	update := expression.Set(isReleasedAttr, isReleasedAttrVal)
	if len(data) > 0 {
		update = update.Set(dataAttr, expression.Value(data))
	}
	updateExpr, _ := expression.NewBuilder().WithUpdate(update).WithCondition(ownershipLockCond).Build()

	updateItemRequest := &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.tableName),
		Key:                       key,
		UpdateExpression:          updateExpr.Update(),
		ConditionExpression:       updateExpr.Condition(),
		ExpressionAttributeNames:  updateExpr.Names(),
		ExpressionAttributeValues: updateExpr.Values(),
	}

	_, err := c.dynamoDB.UpdateItem(ctx, updateItemRequest)
	return err
}

func (c *commonClient) releaseAllLocks(ctx context.Context) error {
	var err error
	c.locks.Range(func(key interface{}, value interface{}) bool {
		err = c.releaseLock(ctx, value.(*Lock))
		return err == nil
	})
	return err
}

func (c *commonClient) getItemKeys(lockItem *Lock) map[string]types.AttributeValue {
	key := map[string]types.AttributeValue{
		c.partitionKeyName: stringAttrValue(lockItem.partitionKey),
	}
	return key
}

func (c *commonClient) get(ctx context.Context, partitionKey string) (*Lock, error) {
	if c.isClosed() {
		return nil, ErrClientClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	getLockOption := getLockOptions{
		partitionKey: partitionKey,
	}
	keyName := getLockOption.partitionKey
	v, ok := c.locks.Load(keyName)
	if ok {
		return v.(*Lock), nil
	}

	lockItem, err := c.getLockFromDynamoDB(ctx, getLockOption)
	if err != nil {
		return nil, err
	}

	if lockItem == nil {
		return &Lock{}, nil
	}

	lockItem.updateRVN("", time.Time{}, lockItem.leaseDuration)
	return lockItem, nil
}

// ErrClientClosed reports the client cannot be used because it is already
// closed.
var ErrClientClosed = errors.New("client already closed")

func (c *commonClient) isClosed() bool {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	return closed
}

// Close releases all of the locks. The given context is passed down
// to the underlying dynamoDB calls.
func (c *commonClient) Close(ctx context.Context) error {
	err := ErrClientClosed
	c.closeOnce.Do(func() {
		// Hold the write lock for the duration of the close operation
		// to prevent new locks from being acquired.
		c.mu.Lock()
		defer c.mu.Unlock()
		err = c.releaseAllLocks(ctx)
		c.stopHeartbeat()
		c.closed = true
	})
	return err
}

func (c *commonClient) tryAddSessionMonitor(lockName string, lock *Lock) {
	if lock.sessionMonitor != nil && lock.sessionMonitor.callback != nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.lockSessionMonitorChecker(ctx, lockName, lock)
		c.sessionMonitorCancellations.Store(lockName, cancel)
	}
}

func (c *commonClient) removeKillSessionMonitor(monitorName string) {
	sm, ok := c.sessionMonitorCancellations.Load(monitorName)
	if !ok {
		return
	}
	if cancel, ok := sm.(func()); ok {
		cancel()
	} else if cancel, ok := sm.(context.CancelFunc); ok {
		cancel()
	}
}

func (c *commonClient) lockSessionMonitorChecker(ctx context.Context,
	monitorName string, lock *Lock) {
	go func() {
		defer c.sessionMonitorCancellations.Delete(monitorName)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				timeUntilDangerZone, err := lock.timeUntilDangerZoneEntered()
				if err != nil {
					c.logger.Error(ctx, "cannot run session monitor because", err)
					return
				}
				if timeUntilDangerZone <= 0 {
					go lock.sessionMonitor.callback()
					return
				}
				time.Sleep(timeUntilDangerZone)
			}
		}
	}()
}

func stringAttrValue(s string) *types.AttributeValueMemberS {
	return &types.AttributeValueMemberS{Value: s}
}

func bytesAttrValue(b []byte) *types.AttributeValueMemberB {
	return &types.AttributeValueMemberB{Value: b}
}

func readStringAttr(attr types.AttributeValue) string {
	if s, ok := attr.(*types.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func readBytesAttr(attr types.AttributeValue) []byte {
	if b, ok := attr.(*types.AttributeValueMemberB); ok {
		return b.Value
	}
	return nil
}

// DynamoDBClient defines the public interface that must be fulfilled for
// testing doubles.
type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	CreateTable(ctx context.Context, params *dynamodb.CreateTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
}
