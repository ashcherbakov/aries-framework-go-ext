/*
Copyright Scoir Inc Technologies Inc, SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package mongodb implements a storage provider conforming to the storage interface in aries-framework-go.
// It is compatible with MongoDB v4.0.0, v4.2.8, and v5.0.0. It is also compatible with Amazon DocumentDB 4.0.0.
// It may be compatible with other versions, but they haven't been tested.
package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hyperledger/aries-framework-go/spi/storage"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	mongooptions "go.mongodb.org/mongo-driver/mongo/options"
)

const (
	defaultTimeout                         = time.Second * 10
	defaultMaxIndexCreationConflictRetries = 3

	invalidTagName                       = `"%s" is an invalid tag name since it contains one or more ':' characters`
	invalidTagValue                      = `"%s" is an invalid tag value since it contains one or more ':' characters`
	failCreateIndexesInMongoDBCollection = "failed to create indexes in MongoDB collection: %w"

	expressionTagNameOnlyLength     = 1
	expressionTagNameAndValueLength = 2
	andExpressionLength
)

var errInvalidQueryExpressionFormat = errors.New("invalid expression format. " +
	"It must be in the following format: " +
	"TagName:TagValue or TagName1:TagValue1&&TagName2:TagValue2. Tag values are optional")

type logger interface {
	Infof(msg string, args ...interface{})
}

type defaultLogger struct {
	logger *log.Logger
}

func (d *defaultLogger) Infof(msg string, args ...interface{}) {
	d.logger.Printf(msg, args...)
}

type closer func(storeName string)

type dataWrapper struct {
	Key  string                 `bson:"_id"`
	Doc  map[string]interface{} `bson:"doc,omitempty"`
	Str  string                 `bson:"str,omitempty"`
	Bin  []byte                 `bson:"bin,omitempty"`
	Tags map[string]interface{} `bson:"tags,omitempty"`
}

// Option represents an option for a MongoDB Provider.
type Option func(opts *Provider)

// WithDBPrefix is an option for adding a prefix to all created database names.
// No prefix will be used by default.
func WithDBPrefix(dbPrefix string) Option {
	return func(opts *Provider) {
		opts.dbPrefix = dbPrefix
	}
}

// WithLogger is an option for specifying a custom logger.
// The standard Golang logger will be used by default.
func WithLogger(logger logger) Option {
	return func(opts *Provider) {
		opts.logger = logger
	}
}

// WithTimeout is an option for specifying the timeout for all calls to MongoDB.
// The timeout is 10 seconds by default.
func WithTimeout(timeout time.Duration) Option {
	return func(opts *Provider) {
		opts.timeout = timeout
	}
}

// WithMaxRetries is an option for specifying how many retries are allowed when there are certain transient errors
// from MongoDB. These transient errors can happen in two situations:
// 1. An index conflict error when setting indexes via the SetStoreConfig method from multiple MongoDB Provider
//    objects that look at the same stores (which might happen if you have multiple running instances of a service).
// 2. If you're using MongoDB 4.0.0 (or DocumentDB 4.0.0), a "dup key" type of error when calling store.Put or
//    store.Batch from multiple MongoDB Provider objects that look at the same stores.
// maxRetries must be > 0. If not set (or set to an invalid value), it will default to 3.
func WithMaxRetries(maxRetries uint64) Option {
	return func(opts *Provider) {
		opts.maxRetries = maxRetries
	}
}

// WithTimeBetweenRetries is an option for specifying how long to wait between retries when
// there are certain transient errors from MongoDB. These transient errors can happen in two situations:
// 1. An index conflict error when setting indexes via the SetStoreConfig method from multiple MongoDB Provider
//    objects that look at the same stores (which might happen if you have multiple running instances of a service).
// 2. If you're using MongoDB 4.0.0 (or DocumentDB 4.0.0), a "dup key" type of error when calling store.Put or
//    store.Batch multiple times in parallel on the same key.
// Defaults to two seconds if not set.
func WithTimeBetweenRetries(timeBetweenRetries time.Duration) Option {
	return func(opts *Provider) {
		opts.timeBetweenRetries = timeBetweenRetries
	}
}

// Provider represents a MongoDB/DocumentDB implementation of the storage.Provider interface.
type Provider struct {
	client             *mongo.Client
	openStores         map[string]*store
	dbPrefix           string
	lock               sync.RWMutex
	logger             logger
	timeout            time.Duration
	maxRetries         uint64
	timeBetweenRetries time.Duration
}

// NewProvider instantiates a new MongoDB Provider.
// connString is a connection string as defined in https://docs.mongodb.com/manual/reference/connection-string/.
// Note that options supported by the Go Mongo driver (and the names of them) may differ from the documentation above.
// Check the Go Mongo driver (go.mongodb.org/mongo-driver/mongo) to make sure the options you're specifying
// are supported and will be captured correctly.
// If using DocumentDB, the retryWrites option must be set to false in the connection string (retryWrites=false) in
// order for it to work.
func NewProvider(connString string, opts ...Option) (*Provider, error) {
	p := &Provider{openStores: map[string]*store{}}

	setOptions(opts, p)

	client, err := mongo.NewClient(mongooptions.Client().ApplyURI(connString))
	if err != nil {
		return nil, fmt.Errorf("failed to create a new MongoDB client: %w", err)
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	err = client.Connect(ctxWithTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	p.client = client

	return p, nil
}

// OpenStore opens a Store with the given name and returns a handle.
// If the underlying database for the given name has never been created before, then it is created.
// Store names are not case-sensitive. If name is blank, then an error will be returned.
func (p *Provider) OpenStore(name string) (storage.Store, error) {
	if name == "" {
		return nil, fmt.Errorf("store name cannot be empty")
	}

	name = strings.ToLower(p.dbPrefix + name)

	p.lock.Lock()
	defer p.lock.Unlock()

	openStore, ok := p.openStores[name]
	if ok {
		return openStore, nil
	}

	newStore := &store{
		// The storage interface doesn't have the concept of a nested database, so we have no real use for the
		// collection abstraction MongoDB uses. Since we have to use at least one collection, we keep the collection
		// name as short as possible to avoid hitting the index size limit.
		coll:               p.getCollectionHandle(name),
		name:               name,
		logger:             p.logger,
		close:              p.removeStore,
		timeout:            p.timeout,
		maxRetries:         p.maxRetries,
		timeBetweenRetries: p.timeBetweenRetries,
	}

	p.openStores[name] = newStore

	return newStore, nil
}

// SetStoreConfig sets the configuration on a store.
// Indexes are created based on the tag names in config. This allows the store.Query method to operate faster.
// Existing tag names/indexes in the store that are not in the config passed in here will be removed.
// The store must already be open in this provider from a prior call to OpenStore. The name parameter cannot be blank.
func (p *Provider) SetStoreConfig(storeName string, config storage.StoreConfiguration) error {
	for _, tagName := range config.TagNames {
		if strings.Contains(tagName, ":") {
			return fmt.Errorf(invalidTagName, tagName)
		}
	}

	storeName = strings.ToLower(p.dbPrefix + storeName)

	openStore, found := p.openStores[storeName]
	if !found {
		return storage.ErrStoreNotFound
	}

	var attemptsMade int

	err := backoff.Retry(func() error {
		attemptsMade++

		err := p.setIndexes(openStore, config)
		if err != nil {
			// If there are multiple MongoDB Providers trying to set store configurations, it's possible
			// to get an error. In cases where those multiple MongoDB providers are trying
			// to set the exact same store configuration, retrying here allows them to succeed without failing
			// unnecessarily.
			if isIndexConflictErrorMessage(err) {
				p.logger.Infof("[Store name: %s] Attempt %d - error while setting indexes. "+
					"This can happen if multiple MongoDB providers set the store configuration at the "+
					"same time. If there are remaining retries, this operation will be tried again after %s. "+
					"Underlying error message: %s",
					storeName, attemptsMade, p.timeBetweenRetries.String(), err.Error())

				// The error below isn't marked using backoff.Permanent, so it'll only be seen if the retry limit
				// is reached.
				return fmt.Errorf("failed to set indexes after %d attempts. This storage provider may "+
					"need to be started with a higher max retry limit and/or higher time between retries. "+
					"Underlying error message: %w", attemptsMade, err)
			}

			// This is an unexpected error.
			return backoff.Permanent(fmt.Errorf("failed to set indexes: %w", err))
		}

		p.logger.Infof("[Store name: %s] Attempt %d - successfully set indexes.",
			storeName, attemptsMade)

		return nil
	}, backoff.WithMaxRetries(backoff.NewConstantBackOff(p.timeBetweenRetries), p.maxRetries))
	if err != nil {
		return err
	}

	return nil
}

// GetStoreConfig gets the current Store configuration.
// If the underlying database for the given name has never been
// created by a call to OpenStore at some point, then an error wrapping ErrStoreNotFound will be returned. This
// method will not open a store in the Provider.
// If name is blank, then an error will be returned.
func (p *Provider) GetStoreConfig(name string) (storage.StoreConfiguration, error) {
	name = strings.ToLower(p.dbPrefix + name)

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	databaseNames, err := p.client.ListDatabaseNames(ctxWithTimeout, bson.D{{Key: "name", Value: name}})
	if err != nil {
		return storage.StoreConfiguration{}, fmt.Errorf("failed to determine if the underlying database "+
			"exists for %s: %w", name, err)
	}

	if len(databaseNames) == 0 {
		return storage.StoreConfiguration{}, storage.ErrStoreNotFound
	}

	existingIndexedTagNames, err := p.getExistingIndexedTagNames(p.getCollectionHandle(name))
	if err != nil {
		return storage.StoreConfiguration{}, fmt.Errorf("failed to get existing indexed tag names: %w", err)
	}

	return storage.StoreConfiguration{TagNames: existingIndexedTagNames}, nil
}

// GetOpenStores returns all Stores currently open in this Provider.
func (p *Provider) GetOpenStores() []storage.Store {
	p.lock.RLock()
	defer p.lock.RUnlock()

	openStores := make([]storage.Store, len(p.openStores))

	var counter int

	for _, openStore := range p.openStores {
		openStores[counter] = openStore
		counter++
	}

	return openStores
}

// Close closes all stores created under this store provider.
func (p *Provider) Close() error {
	p.lock.RLock()

	openStoresSnapshot := make([]*store, len(p.openStores))

	var counter int

	for _, openStore := range p.openStores {
		openStoresSnapshot[counter] = openStore
		counter++
	}
	p.lock.RUnlock()

	for _, openStore := range openStoresSnapshot {
		err := openStore.Close()
		if err != nil {
			return fmt.Errorf(`failed to close open store with name "%s": %w`, openStore.name, err)
		}
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	err := p.client.Disconnect(ctxWithTimeout)
	if err != nil {
		if err.Error() == "client is disconnected" {
			return nil
		}

		return fmt.Errorf("failed to disconnect from MongoDB: %w", err)
	}

	return nil
}

func (p *Provider) removeStore(name string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	_, ok := p.openStores[name]
	if ok {
		delete(p.openStores, name)
	}
}

func (p *Provider) getCollectionHandle(name string) *mongo.Collection {
	return p.client.Database(name).Collection("c")
}

func (p *Provider) setIndexes(openStore *store, config storage.StoreConfiguration) error {
	tagNamesNeedIndexCreation, err := p.determineTagNamesNeedIndexCreation(openStore, config)
	if err != nil {
		return err
	}

	if len(tagNamesNeedIndexCreation) > 0 {
		models := make([]mongo.IndexModel, len(tagNamesNeedIndexCreation))

		for i, tagName := range tagNamesNeedIndexCreation {
			indexOptions := mongooptions.Index()
			indexOptions.SetName(tagName)

			models[i] = mongo.IndexModel{
				Keys:    bson.D{{Key: fmt.Sprintf("tags.%s", tagName), Value: 1}},
				Options: indexOptions,
			}
		}

		err = p.createIndexes(openStore, models)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Provider) determineTagNamesNeedIndexCreation(openStore *store,
	config storage.StoreConfiguration) ([]string, error) {
	existingIndexedTagNames, err := p.getExistingIndexedTagNames(openStore.coll)
	if err != nil {
		return nil, fmt.Errorf("failed to get existing indexed tag names: %w", err)
	}

	tagNameIndexesAlreadyConfigured := make(map[string]struct{})

	for _, existingIndexedTagName := range existingIndexedTagNames {
		var existingTagIsInNewConfig bool

		for _, tagName := range config.TagNames {
			if existingIndexedTagName == tagName {
				existingTagIsInNewConfig = true
				tagNameIndexesAlreadyConfigured[tagName] = struct{}{}

				p.logger.Infof("[Store name (includes prefix, if any): %s] Skipping index creation for %s "+
					"since the index already exists.", openStore.name, tagName)

				break
			}
		}

		// If the new store configuration doesn't have the existing index (tag) defined, then we will delete it
		if !existingTagIsInNewConfig {
			ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)

			_, errDrop := openStore.coll.Indexes().DropOne(ctxWithTimeout, existingIndexedTagName)
			if errDrop != nil {
				cancel()

				return nil, fmt.Errorf("failed to remove index for %s: %w", existingIndexedTagName, errDrop)
			}

			cancel()
		}
	}

	var tagNamesNeedIndexCreation []string

	for _, tag := range config.TagNames {
		_, indexAlreadyCreated := tagNameIndexesAlreadyConfigured[tag]
		if !indexAlreadyCreated {
			tagNamesNeedIndexCreation = append(tagNamesNeedIndexCreation, tag)
		}
	}

	return tagNamesNeedIndexCreation, nil
}

func (p *Provider) getExistingIndexedTagNames(collection *mongo.Collection) ([]string, error) {
	indexesCursor, err := p.getIndexesCursor(collection)
	if err != nil {
		return nil, err
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	var results []bson.M

	err = indexesCursor.All(ctxWithTimeout, &results)
	if err != nil {
		return nil, fmt.Errorf("failed to get all results from indexes cursor")
	}

	if results == nil {
		return nil, nil
	}

	existingIndexedTagNames := make([]string, len(results)-1)

	var counter int

	for _, result := range results {
		indexNameRaw, exists := result["name"]
		if !exists {
			return nil, errors.New(`index data is missing the "key" field`)
		}

		indexName, ok := indexNameRaw.(string)
		if !ok {
			return nil, errors.New(`index name is of unexpected type`)
		}

		// The _id_ index is a built-in index in MongoDB. It wasn't one that can be set using SetStoreConfig,
		// so we omit it here.
		if indexName == "_id_" {
			continue
		}

		existingIndexedTagNames[counter] = indexName

		counter++
	}

	return existingIndexedTagNames, nil
}

func (p *Provider) getIndexesCursor(collection *mongo.Collection) (*mongo.Cursor, error) {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	indexesCursor, err := collection.Indexes().List(ctxWithTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of indexes from MongoDB: %w", err)
	}

	return indexesCursor, nil
}

func (p *Provider) createIndexes(openStore *store, models []mongo.IndexModel) error {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	_, err := openStore.coll.Indexes().CreateMany(ctxWithTimeout, models)
	if err != nil {
		return fmt.Errorf(failCreateIndexesInMongoDBCollection, err)
	}

	return nil
}

type store struct {
	name               string
	logger             logger
	coll               *mongo.Collection
	close              closer
	timeout            time.Duration
	maxRetries         uint64
	timeBetweenRetries time.Duration
}

// Put stores the key + value pair along with the (optional) tags.
// If tag values are valid int32 or int64, they will be stored as integers in MongoDB, so we can sort numerically later.
func (s *store) Put(key string, value []byte, tags ...storage.Tag) error {
	err := validatePutInput(key, value, tags)
	if err != nil {
		return err
	}

	data := generateDataWrapper(key, value, tags)

	return s.executeUpdateOneCommand(key, data)
}

func (s *store) Get(k string) ([]byte, error) {
	if k == "" {
		return nil, errors.New("key is mandatory")
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	result := s.coll.FindOne(ctxWithTimeout, bson.M{"_id": k})
	if errors.Is(result.Err(), mongo.ErrNoDocuments) {
		return nil, storage.ErrDataNotFound
	} else if result.Err() != nil {
		return nil, fmt.Errorf("failed to run FindOne command in MongoDB: %w", result.Err())
	}

	_, value, err := getKeyAndValueFromMongoDBResult(result)
	if err != nil {
		return nil, fmt.Errorf("failed to get value from MongoDB result: %w", err)
	}

	return value, nil
}

func (s *store) GetTags(key string) ([]storage.Tag, error) {
	if key == "" {
		return nil, errors.New("key is mandatory")
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	result := s.coll.FindOne(ctxWithTimeout, bson.M{"_id": key})
	if errors.Is(result.Err(), mongo.ErrNoDocuments) {
		return nil, storage.ErrDataNotFound
	} else if result.Err() != nil {
		return nil, fmt.Errorf("failed to run FindOne command in MongoDB: %w", result.Err())
	}

	tags, err := getTagsFromMongoDBResult(result)
	if err != nil {
		return nil, fmt.Errorf("failed to get tags from MongoDB result: %w", err)
	}

	return tags, nil
}

func (s *store) GetBulk(keys ...string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("keys slice must contain at least one key")
	}

	for _, key := range keys {
		if key == "" {
			return nil, errors.New("key cannot be empty")
		}
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	cursor, err := s.coll.Find(ctxWithTimeout, bson.M{"_id": bson.D{
		{Key: "$in", Value: keys},
	}})
	if err != nil {
		return nil, fmt.Errorf("failed to run Find command in MongoDB: %w", err)
	}

	allValues, err := s.collectBulkGetResults(keys, cursor)
	if err != nil {
		return nil, err
	}

	return allValues, nil
}

// Query does a query for data as defined by the documentation in storage.Store (the interface).
// This implementation also supports querying for data tagged with two tag name + value pairs (using AND logic).
// To do this, separate the tag name + value pairs using &&. You can still omit one or both of the tag values
// in order to indicate that you want any data tagged with the tag name, regardless of tag value.
// For example, TagName1:TagValue1&&TagName2:TagValue2 will return only data that has been tagged with both pairs.
// See testQueryWithMultipleTags in store_test.go for more examples of querying using multiple tags.
// It's recommended to set up an index using the Provider.SetStoreConfig method in order to speed up queries.
// TODO (#146) Investigate compound indexes and see if they may be useful for queries with sorts and/or for queries
//             with multiple tags.
func (s *store) Query(expression string, options ...storage.QueryOption) (storage.Iterator, error) {
	if expression == "" {
		return &iterator{}, errInvalidQueryExpressionFormat
	}

	expressionSplitByANDOperator := strings.Split(expression, "&&")

	var filter bson.D

	var err error

	switch len(expressionSplitByANDOperator) {
	case 1:
		filter, err = prepareSimpleFilter(expression)
		if err != nil {
			return nil, err
		}
	case andExpressionLength:
		filter, err = prepareANDFilter(expressionSplitByANDOperator)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errInvalidQueryExpressionFormat
	}

	findOptions := s.createMongoDBFindOptions(options)

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	cursor, err := s.coll.Find(ctxWithTimeout, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to run Find command in MongoDB: %w", err)
	}

	return &iterator{
		cursor:  cursor,
		coll:    s.coll,
		filter:  filter,
		timeout: s.timeout,
	}, nil
}

// Delete deletes the value (and all tags) associated with key.
func (s *store) Delete(key string) error {
	if key == "" {
		return errors.New("key is mandatory")
	}

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	_, err := s.coll.DeleteOne(ctxWithTimeout, bson.M{"_id": key})
	if err != nil {
		return fmt.Errorf("failed to run DeleteOne command in MongoDB: %w", err)
	}

	return err
}

func (s *store) Batch(operations []storage.Operation) error {
	if len(operations) == 0 {
		return errors.New("batch requires at least one operation")
	}

	for _, operation := range operations {
		if operation.Key == "" {
			return errors.New("key cannot be empty")
		}
	}

	models := make([]mongo.WriteModel, len(operations))

	for i, operation := range operations {
		models[i] = generateModelForBulkWriteCall(operation)
	}

	return s.executeBulkWriteCommand(models)
}

// Flush doesn't do anything since this store type doesn't queue values.
func (s *store) Flush() error {
	return nil
}

// Close removes this store from the parent Provider's list of open stores. It does not close this store's connection
// to the database, since it's shared across stores. To close the connection you must call Provider.Close.
func (s *store) Close() error {
	s.close(s.name)

	return nil
}

func (s *store) executeUpdateOneCommand(key string, dataWrapperToStore dataWrapper) error {
	opts := mongooptions.UpdateOptions{}
	opts.SetUpsert(true)

	var attemptsMade int

	return backoff.Retry(func() error {
		attemptsMade++

		ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		_, err := s.coll.UpdateOne(ctxWithTimeout, bson.M{"_id": key}, bson.M{"$set": dataWrapperToStore}, &opts)
		if err != nil {
			// If using MongoDB 4.0.0 (or DocumentDB 4.0.0), and this is called multiple times in parallel on the
			// same key, then it's possible to get a transient error here. We need to retry in this case.
			if strings.Contains(err.Error(), "duplicate key error collection") {
				s.logger.Infof(`[Store name: %s] Attempt %d - error while storing data under key "%s". `+
					"This can happen if there are multiple calls in parallel to store data under the same key. "+
					"If there are remaining retries, this operation will be tried again after %s. "+
					"Underlying error message: %s", s.name, attemptsMade, key, s.timeBetweenRetries.String(),
					err.Error())

				// The error below isn't marked using backoff.Permanent, so it'll only be seen if the retry limit
				// is reached.
				return fmt.Errorf("failed to store data after %d attempts. This storage provider may "+
					"need to be started with a higher max retry limit and/or higher time between retries. "+
					"Underlying error message: %w", attemptsMade, err)
			}

			// This is an unexpected error.
			return backoff.Permanent(fmt.Errorf("failed to run UpdateOne command in MongoDB: %w", err))
		}

		return nil
	}, backoff.WithMaxRetries(backoff.NewConstantBackOff(s.timeBetweenRetries), s.maxRetries))
}

func (s *store) collectBulkGetResults(keys []string, cursor *mongo.Cursor) ([][]byte, error) {
	allValues := make([][]byte, len(keys))

	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	for cursor.Next(ctxWithTimeout) {
		key, value, err := getKeyAndValueFromMongoDBResult(cursor)
		if err != nil {
			return nil, fmt.Errorf("failed to get value from MongoDB result: %w", err)
		}

		for i := 0; i < len(keys); i++ {
			if key == keys[i] {
				allValues[i] = value

				break
			}
		}
	}

	return allValues, nil
}

func (s *store) executeBulkWriteCommand(models []mongo.WriteModel) error {
	var attemptsMade int

	return backoff.Retry(func() error {
		attemptsMade++

		ctxWithTimeout, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()

		_, err := s.coll.BulkWrite(ctxWithTimeout, models)
		if err != nil {
			// If using MongoDB 4.0.0 (or DocumentDB 4.0.0), and this is called multiple times in parallel on the
			// same key(s), then it's possible to get a transient error here. We need to retry in this case.
			if strings.Contains(err.Error(), "duplicate key error collection") {
				s.logger.Infof(`[Store name: %s] Attempt %d - error while performing batch operations. `+
					"This can happen if there are multiple calls in parallel to do batch operations under the "+
					"same key(s). If there are remaining retries, the batch operations will be tried again after %s. "+
					"Underlying error message: %s", s.name, attemptsMade, s.timeBetweenRetries.String(), err.Error())

				// The error below isn't marked using backoff.Permanent, so it'll only be seen if the retry limit
				// is reached.
				return fmt.Errorf("failed to perform batch operations after %d attempts. This storage provider "+
					"may need to be started with a higher max retry limit and/or higher time between retries. "+
					"Underlying error message: %w", attemptsMade, err)
			}

			// This is an unexpected error.
			return backoff.Permanent(fmt.Errorf("failed to run BulkWrite command in MongoDB: %w", err))
		}

		return nil
	}, backoff.WithMaxRetries(backoff.NewConstantBackOff(s.timeBetweenRetries), s.maxRetries))
}

func (s *store) createMongoDBFindOptions(options []storage.QueryOption) *mongooptions.FindOptions {
	queryOptions := getQueryOptions(options)

	findOptions := mongooptions.Find()

	if queryOptions.PageSize > 0 || queryOptions.InitialPageNum > 0 {
		findOptions = mongooptions.Find()

		findOptions.SetBatchSize(int32(queryOptions.PageSize))

		if queryOptions.PageSize > 0 && queryOptions.InitialPageNum > 0 {
			findOptions.SetSkip(int64(queryOptions.InitialPageNum * queryOptions.PageSize))
		}
	}

	if queryOptions.SortOptions != nil {
		mongoDBSortOrder := 1
		if queryOptions.SortOptions.Order == storage.SortDescending {
			mongoDBSortOrder = -1
		}

		findOptions.SetSort(bson.D{{
			Key:   fmt.Sprintf("tags.%s", queryOptions.SortOptions.TagName),
			Value: mongoDBSortOrder,
		}})
	}

	return findOptions
}

type iterator struct {
	cursor  *mongo.Cursor
	coll    *mongo.Collection
	filter  bson.D
	timeout time.Duration
}

func (i *iterator) Next() (bool, error) {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	return i.cursor.Next(ctxWithTimeout), nil
}

func (i *iterator) Key() (string, error) {
	key, _, err := getKeyAndValueFromMongoDBResult(i.cursor)
	if err != nil {
		return "", fmt.Errorf("failed to get key from MongoDB result: %w", err)
	}

	return key, nil
}

func (i *iterator) Value() ([]byte, error) {
	_, value, err := getKeyAndValueFromMongoDBResult(i.cursor)
	if err != nil {
		return nil, fmt.Errorf("failed to get value from MongoDB result: %w", err)
	}

	return value, nil
}

func (i *iterator) Tags() ([]storage.Tag, error) {
	tags, err := getTagsFromMongoDBResult(i.cursor)
	if err != nil {
		return nil, fmt.Errorf("failed to get tags from MongoDB result: %w", err)
	}

	return tags, nil
}

// TODO (#147) Investigate using aggregates to get total items without doing a separate query.

func (i *iterator) TotalItems() (int, error) {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	totalItems, err := i.coll.CountDocuments(ctxWithTimeout, i.filter)
	if err != nil {
		return -1, fmt.Errorf("failed to get document count from MongoDB: %w", err)
	}

	return int(totalItems), nil
}

func (i *iterator) Close() error {
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), i.timeout)
	defer cancel()

	return i.cursor.Close(ctxWithTimeout)
}

func setOptions(opts []Option, p *Provider) {
	for _, opt := range opts {
		opt(p)
	}

	if p.logger == nil {
		p.logger = &defaultLogger{
			log.New(os.Stdout, "MongoDB-Provider ", log.Ldate|log.Ltime|log.LUTC),
		}
	}

	if p.timeout == 0 {
		p.timeout = defaultTimeout
	}

	if p.maxRetries < 1 {
		p.maxRetries = defaultMaxIndexCreationConflictRetries
	}
}

func isIndexConflictErrorMessage(err error) bool {
	// DocumentDB may return either of these two error message.
	documentDBPossibleErrMsg1 := "Non-unique"
	documentDBPossibleErrMsg2 := "Existing index build in progress on the same collection. " +
		"Collection is limited to a single index build at a time."
	documentDBPossibleErrMsg3 := "EOF"
	// MongoDB 5.0.0 may return this error message.
	mongoDB500PossibleErrMsg := "incomplete read of message header"

	if strings.Contains(err.Error(), documentDBPossibleErrMsg1) ||
		strings.Contains(err.Error(), documentDBPossibleErrMsg2) ||
		strings.Contains(err.Error(), documentDBPossibleErrMsg3) ||
		strings.Contains(err.Error(), mongoDB500PossibleErrMsg) {
		return true
	}

	return false
}

func validatePutInput(key string, value []byte, tags []storage.Tag) error {
	if key == "" {
		return errors.New("key cannot be empty")
	}

	if value == nil {
		return errors.New("value cannot be nil")
	}

	for _, tag := range tags {
		if strings.Contains(tag.Name, ":") {
			return fmt.Errorf(invalidTagName, tag.Name)
		}

		if strings.Contains(tag.Value, ":") {
			return fmt.Errorf(invalidTagValue, tag.Value)
		}
	}

	return nil
}

func convertTagSliceToMap(tagSlice []storage.Tag) map[string]interface{} {
	tagsMap := make(map[string]interface{})

	for _, tag := range tagSlice {
		tagsMap[tag.Name] = convertToIntIfPossible(tag.Value)
	}

	return tagsMap
}

// If possible, converts value to an int and returns it.
// Otherwise, it returns value as a string, untouched.
func convertToIntIfPossible(value string) interface{} {
	valueAsInt, err := strconv.Atoi(value)
	if err != nil {
		return value
	}

	return valueAsInt
}

func convertTagMapToSlice(tagMap map[string]interface{}) []storage.Tag {
	tagsSlice := make([]storage.Tag, len(tagMap))

	var counter int

	for tagName, tagValue := range tagMap {
		tagsSlice[counter] = storage.Tag{
			Name:  tagName,
			Value: fmt.Sprintf("%v", tagValue),
		}

		counter++
	}

	return tagsSlice
}

type decoder interface {
	Decode(interface{}) error
}

func getKeyAndValueFromMongoDBResult(decoder decoder) (key string, value []byte, err error) {
	data, errGetDataWrapper := getDataWrapperFromMongoDBResult(decoder)
	if errGetDataWrapper != nil {
		return "", nil, fmt.Errorf("failed to get data wrapper from MongoDB result: %w", errGetDataWrapper)
	}

	if data.Doc != nil {
		dataBytes, errMarshal := json.Marshal(data.Doc)
		if errMarshal != nil {
			return "", nil, fmt.Errorf("failed to marshal value into bytes: %w", errMarshal)
		}

		return data.Key, dataBytes, nil
	}

	if data.Bin != nil {
		return data.Key, data.Bin, nil
	}

	valueBytes, err := json.Marshal(data.Str)
	if err != nil {
		return "", nil, fmt.Errorf("marshal string value: %w", err)
	}

	return data.Key, valueBytes, nil
}

func getTagsFromMongoDBResult(decoder decoder) ([]storage.Tag, error) {
	data, err := getDataWrapperFromMongoDBResult(decoder)
	if err != nil {
		return nil, fmt.Errorf("failed to get data wrapper from MongoDB result: %w", err)
	}

	return convertTagMapToSlice(data.Tags), nil
}

// getDataWrapperFromMongoDBResult unmarshals and returns a dataWrapper from the MongoDB result.
func getDataWrapperFromMongoDBResult(decoder decoder) (*dataWrapper, error) {
	data := &dataWrapper{}

	if err := decoder.Decode(data); err != nil {
		return nil, fmt.Errorf("failed to decode data from MongoDB: %w", err)
	}

	return data, nil
}

func getQueryOptions(options []storage.QueryOption) storage.QueryOptions {
	var queryOptions storage.QueryOptions

	for _, option := range options {
		if option != nil {
			option(&queryOptions)
		}
	}

	if queryOptions.InitialPageNum < 0 {
		queryOptions.InitialPageNum = 0
	}

	return queryOptions
}

func prepareSimpleFilter(expression string) (bson.D, error) {
	operand, err := prepareSingleOperand(expression)
	if err != nil {
		return nil, err
	}

	return bson.D{operand}, nil
}

func prepareANDFilter(expressionSplitByANDOperator []string) (bson.D, error) {
	operand1, err := prepareSingleOperand(expressionSplitByANDOperator[0])
	if err != nil {
		return nil, err
	}

	operand2, err := prepareSingleOperand(expressionSplitByANDOperator[1])
	if err != nil {
		return nil, err
	}

	// When the bson.D below gets serialized, it'll be comma separated.
	// MongoDB treats a comma separated list of expression as an implicit AND operation.
	return bson.D{operand1, operand2}, nil
}

func prepareSingleOperand(expression string) (bson.E, error) {
	expressionSplitByColon := strings.Split(expression, ":")

	var filterValue interface{}

	switch len(expressionSplitByColon) {
	case expressionTagNameOnlyLength:
		filterValue = bson.D{
			{Key: "$exists", Value: true},
		}
	case expressionTagNameAndValueLength:
		filterValue = convertToIntIfPossible(expressionSplitByColon[1])
	default:
		return bson.E{}, errInvalidQueryExpressionFormat
	}

	operand := bson.E{
		Key:   fmt.Sprintf("tags.%s", expressionSplitByColon[0]),
		Value: filterValue,
	}

	return operand, nil
}

func generateModelForBulkWriteCall(operation storage.Operation) mongo.WriteModel {
	if operation.Value == nil {
		return mongo.NewDeleteOneModel().SetFilter(bson.M{"_id": operation.Key})
	}

	data := generateDataWrapper(operation.Key, operation.Value, operation.Tags)

	return mongo.NewUpdateOneModel().
		SetFilter(bson.M{"_id": operation.Key}).
		SetUpdate(bson.M{"$set": data}).
		SetUpsert(true)
}

func generateDataWrapper(key string, value []byte, tags []storage.Tag) dataWrapper {
	tagsAsMap := convertTagSliceToMap(tags)

	data := dataWrapper{
		Key:  key,
		Tags: tagsAsMap,
	}

	var unmarshalledValue map[string]interface{}

	jsonDecoder := json.NewDecoder(bytes.NewReader(value))
	jsonDecoder.UseNumber()

	err := jsonDecoder.Decode(&unmarshalledValue)
	if err == nil {
		data.Doc = unmarshalledValue
	} else {
		var unmarshalledStringValue string

		err = json.Unmarshal(value, &unmarshalledStringValue)
		if err == nil {
			data.Str = unmarshalledStringValue
		} else {
			data.Bin = value
		}
	}

	return data
}
