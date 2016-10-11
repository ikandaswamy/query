//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package system

import (
	"fmt"
	"sync"
	"time"

	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/timestamp"
	"github.com/couchbase/query/value"
)

// A single-entry cache for storing the list of users and roles.
// We are not particularly concerned with performance here, but
// we do want to make sure make multiple requests for the same data within a query.
// This requires us to store the data somewhere after retrieval.
type userRolesCache struct {
	sync.Mutex
	curValue     map[string]value.Value
	whenObtained time.Time
	datastore    datastore.Datastore // where to get data from
}

func (cache *userRolesCache) getNumUsers() (int, errors.Error) {
	cache.Lock()
	defer cache.Unlock()
	err := cache.makeCurrent()
	if err != nil {
		return 0, err
	}
	return len(cache.curValue), nil
}

// Cache should already be locked when this function is called.
func (cache *userRolesCache) makeCurrent() errors.Error {
	if cache.curValue == nil || time.Since(cache.whenObtained).Seconds() > 5.0 {
		// Refresh the cache
		val, err := cache.datastore.UserRoles()
		if err != nil {
			cache.whenObtained = time.Now()
			cache.curValue = nil
			return err
		}
		// Expected data format:
		//   [{"id":"ivanivanov","name":"Ivan Ivanov","roles":[{"role":"cluster_admin"},{"bucket_name":"default","role":"bucket_admin"}]},
		//    {"id":"petrpetrov","name":"Petr Petrov","roles":[{"role":"replication_admin"}]}]
		data := val.Actual()
		sliceOfUsers, ok := data.([]interface{})
		if !ok {
			return errors.NewInvalidValueError(fmt.Sprintf("Unexpected format for user_roles received from server: %v", data))
		}
		newMap := make(map[string]value.Value, len(sliceOfUsers))
		for i, u := range sliceOfUsers {
			userAsMap, ok := u.(map[string]interface{})
			if !ok {
				return errors.NewInvalidValueError(fmt.Sprintf("Unexpected format for user_roles at position %d: %v", i, u))
			}
			id := userAsMap["id"]
			idAsString, ok := id.(string)
			if !ok {
				return errors.NewInvalidValueError(fmt.Sprintf("Could not find id in user_roles data at position %d: %v", i, u))
			}
			newMap[idAsString] = value.NewValue(u)
		}
		cache.whenObtained = time.Now()
		cache.curValue = newMap
	}
	return nil
}

func (cache *userRolesCache) fetch(keys []string) ([]value.AnnotatedPair, []errors.Error) {
	cache.Lock()
	defer cache.Unlock()
	err := cache.makeCurrent()
	if err != nil {
		return nil, []errors.Error{err}
	}

	var errs []errors.Error
	rv := make([]value.AnnotatedPair, 0, len(keys))
	for _, k := range keys {
		val := cache.curValue[k]

		if val == nil {
			if errs == nil {
				errs = make([]errors.Error, 0, 1)
			}
			errs = append(errs, errors.NewNoValueForKey(k))
			continue
		}

		item := value.NewAnnotatedValue(val)
		item.SetAttachment("meta", map[string]interface{}{
			"id": k,
		})

		rv = append(rv, value.AnnotatedPair{
			Name:  k,
			Value: item,
		})
	}

	return rv, errs
}

func (cache *userRolesCache) scanEntries(limit int64, channel datastore.EntryChannel) {
	cache.Lock()
	err := cache.makeCurrent()
	if err != nil {
		cache.Unlock()
		return // No way to report an error here.
	}

	// Put the keys into a temporary store, so we can produce them without holding
	// the lock on userRolesCache. The fetch operator also needs the lock.
	size := limit
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	keys := make([]string, 0, size)
	var numProduced int64 = 0
	for key, _ := range cache.curValue {
		if limit > 0 && numProduced > limit {
			break
		}
		keys = append(keys, key)
		numProduced++
	}
	cache.Unlock()

	for _, k := range keys {
		entry := &datastore.IndexEntry{PrimaryKey: k}
		channel <- entry
	}
}

func newUserRolesCache(ds datastore.Datastore) *userRolesCache {
	return &userRolesCache{datastore: ds}
}

type userRolesKeyspace struct {
	namespace *namespace
	name      string
	indexer   datastore.Indexer
	cache     *userRolesCache
}

func (b *userRolesKeyspace) Release() {
}

func (b *userRolesKeyspace) NamespaceId() string {
	return b.namespace.Id()
}

func (b *userRolesKeyspace) Id() string {
	return b.Name()
}

func (b *userRolesKeyspace) Name() string {
	return b.name
}

func (b *userRolesKeyspace) Count() (int64, errors.Error) {
	v, err := b.cache.getNumUsers()
	return int64(v), err
}

func (b *userRolesKeyspace) Indexer(name datastore.IndexType) (datastore.Indexer, errors.Error) {
	return b.indexer, nil
}

func (b *userRolesKeyspace) Indexers() ([]datastore.Indexer, errors.Error) {
	return []datastore.Indexer{b.indexer}, nil
}

func (b *userRolesKeyspace) Fetch(keys []string) ([]value.AnnotatedPair, []errors.Error) {
	vals, errs := b.cache.fetch(keys)
	return vals, errs
}

func (b *userRolesKeyspace) Insert(inserts []value.Pair) ([]value.Pair, errors.Error) {
	// FIXME
	return nil, errors.NewSystemNotImplementedError(nil, "")
}

func (b *userRolesKeyspace) Update(updates []value.Pair) ([]value.Pair, errors.Error) {
	// FIXME
	return nil, errors.NewSystemNotImplementedError(nil, "")
}

func (b *userRolesKeyspace) Upsert(upserts []value.Pair) ([]value.Pair, errors.Error) {
	// FIXME
	return nil, errors.NewSystemNotImplementedError(nil, "")
}

func (b *userRolesKeyspace) Delete(deletes []string) ([]string, errors.Error) {
	// FIXME
	return nil, errors.NewSystemNotImplementedError(nil, "")
}

func newUserRolesKeyspace(p *namespace) (*userRolesKeyspace, errors.Error) {
	b := new(userRolesKeyspace)
	b.namespace = p
	b.name = KEYSPACE_NAME_USER_ROLES

	primary := &userRolesIndex{name: "#primary", keyspace: b}
	b.indexer = newSystemIndexer(b, primary)

	b.cache = newUserRolesCache(p.store)

	return b, nil
}

type userRolesIndex struct {
	name     string
	keyspace *userRolesKeyspace
}

func (pi *userRolesIndex) KeyspaceId() string {
	return pi.keyspace.Id()
}

func (pi *userRolesIndex) Id() string {
	return pi.Name()
}

func (pi *userRolesIndex) Name() string {
	return pi.name
}

func (pi *userRolesIndex) Type() datastore.IndexType {
	return datastore.DEFAULT
}

func (pi *userRolesIndex) SeekKey() expression.Expressions {
	return nil
}

func (pi *userRolesIndex) RangeKey() expression.Expressions {
	return nil
}

func (pi *userRolesIndex) Condition() expression.Expression {
	return nil
}

func (pi *userRolesIndex) IsPrimary() bool {
	return true
}

func (pi *userRolesIndex) State() (state datastore.IndexState, msg string, err errors.Error) {
	return datastore.ONLINE, "", nil
}

func (pi *userRolesIndex) Statistics(requestId string, span *datastore.Span) (
	datastore.Statistics, errors.Error) {
	return nil, nil
}

func (pi *userRolesIndex) Drop(requestId string) errors.Error {
	return errors.NewSystemIdxNoDropError(nil, "")
}

func (pi *userRolesIndex) Scan(requestId string, span *datastore.Span, distinct bool, limit int64,
	cons datastore.ScanConsistency, vector timestamp.Vector, conn *datastore.IndexConnection) {
	defer close(conn.EntryChannel())

	pi.keyspace.cache.scanEntries(limit, conn.EntryChannel())
}

func (pi *userRolesIndex) ScanEntries(requestId string, limit int64, cons datastore.ScanConsistency,
	vector timestamp.Vector, conn *datastore.IndexConnection) {
	defer close(conn.EntryChannel())

	pi.keyspace.cache.scanEntries(limit, conn.EntryChannel())
}