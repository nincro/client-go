// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"bytes"
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/locate"
	"github.com/tikv/client-go/v2/kv"
	"github.com/tikv/client-go/v2/logutil"
	"github.com/tikv/client-go/v2/retry"
	"github.com/tikv/client-go/v2/tikvrpc"
	zap "go.uber.org/zap"
)

/// GC does garbage collection (GC) of the TiKV cluster.
/// GC deletes MVCC records whose timestamp is lower than the given `safepoint`. We must guarantee
///  that all transactions started before this timestamp had committed. We can keep an active
/// transaction list in application to decide which is the minimal start timestamp of them.
///
/// For each key, the last mutation record (unless it's a deletion) before `safepoint` is retained.
///
/// GC is performed by:
/// 1. resolving all locks with timestamp <= `safepoint`
/// 2. updating PD's known safepoint
///
/// This is a simplified version of [GC in TiDB](https://docs.pingcap.com/tidb/stable/garbage-collection-overview).
/// We skip the second step "delete ranges" which is an optimization for TiDB.
func (s *KVStore) GC(ctx context.Context, safepoint uint64) (newSafePoint uint64, err error) {
	err = s.resolveLocks(ctx, safepoint, 8)
	if err != nil {
		return
	}

	return s.pdClient.UpdateGCSafePoint(ctx, safepoint)
}

func (s *KVStore) resolveLocks(ctx context.Context, safePoint uint64, concurrency int) error {
	handler := func(ctx context.Context, r kv.KeyRange) (RangeTaskStat, error) {
		return s.resolveLocksForRange(ctx, safePoint, r.StartKey, r.EndKey)
	}

	runner := NewRangeTaskRunner("resolve-locks-runner", s, concurrency, handler)
	// Run resolve lock on the whole TiKV cluster. Empty keys means the range is unbounded.
	err := runner.RunOnRange(ctx, []byte(""), []byte(""))
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// We don't want gc to sweep out the cached info belong to other processes, like coprocessor.
const gcScanLockLimit = ResolvedCacheSize / 2

func (s *KVStore) resolveLocksForRange(ctx context.Context, safePoint uint64, startKey []byte, endKey []byte) (RangeTaskStat, error) {
	// for scan lock request, we must return all locks even if they are generated
	// by the same transaction. because gc worker need to make sure all locks have been
	// cleaned.

	var stat RangeTaskStat
	key := startKey
	bo := NewGcResolveLockMaxBackoffer(ctx)
	for {
		select {
		case <-ctx.Done():
			return stat, errors.New("[gc worker] gc job canceled")
		default:
		}

		locks, loc, err := s.scanLocksInRegionWithStartKey(bo, key, safePoint, gcScanLockLimit)
		if err != nil {
			return stat, err
		}

		resolvedLocation, err1 := s.batchResolveLocksInARegion(bo, locks, loc)
		if err1 != nil {
			return stat, errors.Trace(err1)
		}
		// resolve locks failed since the locks are not in one region anymore, need retry.
		if resolvedLocation == nil {
			continue
		}
		if len(locks) < gcScanLockLimit {
			stat.CompletedRegions++
			key = loc.EndKey
			logutil.Logger(ctx).Info("[gc worker] one region finshed ",
				zap.Int("regionID", int(resolvedLocation.Region.GetID())),
				zap.Int("resolvedLocksNum", len(locks)))
		} else {
			logutil.Logger(ctx).Info("[gc worker] region has more than limit locks",
				zap.Int("regionID", int(resolvedLocation.Region.GetID())),
				zap.Int("resolvedLocksNum", len(locks)),
				zap.Int("scan lock limit", gcScanLockLimit))
			key = locks[len(locks)-1].Key
		}

		if len(key) == 0 || (len(endKey) != 0 && bytes.Compare(key, endKey) >= 0) {
			break
		}
		bo = NewGcResolveLockMaxBackoffer(ctx)
	}
	return stat, nil
}

func (s *KVStore) scanLocksInRegionWithStartKey(bo *retry.Backoffer, startKey []byte, maxVersion uint64, limit uint32) (locks []*Lock, loc *locate.KeyLocation, err error) {
	for {
		loc, err := s.GetRegionCache().LocateKey(bo, startKey)
		if err != nil {
			return nil, loc, errors.Trace(err)
		}
		req := tikvrpc.NewRequest(tikvrpc.CmdScanLock, &kvrpcpb.ScanLockRequest{
			MaxVersion: maxVersion,
			Limit:      gcScanLockLimit,
			StartKey:   startKey,
			EndKey:     loc.EndKey,
		})
		resp, err := s.SendReq(bo, req, loc.Region, ReadTimeoutMedium)
		if err != nil {
			return nil, loc, errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return nil, loc, errors.Trace(err)
		}
		if regionErr != nil {
			err = bo.Backoff(BoRegionMiss(), errors.New(regionErr.String()))
			if err != nil {
				return nil, loc, errors.Trace(err)
			}
			continue
		}
		if resp.Resp == nil {
			return nil, loc, errors.Trace(tikverr.ErrBodyMissing)
		}
		locksResp := resp.Resp.(*kvrpcpb.ScanLockResponse)
		if locksResp.GetError() != nil {
			return nil, loc, errors.Errorf("unexpected scanlock error: %s", locksResp)
		}
		locksInfo := locksResp.GetLocks()
		locks = make([]*Lock, len(locksInfo))
		for i := range locksInfo {
			locks[i] = NewLock(locksInfo[i])
		}
		return locks, loc, nil
	}
}

// batchResolveLocksInARegion resolves locks in a region.
// It returns the real location of the resolved locks if resolve locks success.
// It returns error when meet an unretryable error.
// When the locks are not in one region, resolve locks should be failed, it returns with nil resolveLocation and nil err.
// Used it in gcworker only!
func (s *KVStore) batchResolveLocksInARegion(bo *Backoffer, locks []*Lock, expectedLoc *locate.KeyLocation) (resolvedLocation *locate.KeyLocation, err error) {
	resolvedLocation = expectedLoc
	for {
		ok, err := s.GetLockResolver().BatchResolveLocks(bo, locks, resolvedLocation.Region)
		if ok {
			return resolvedLocation, nil
		}
		if err != nil {
			return nil, err
		}
		err = bo.Backoff(retry.BoTxnLock, errors.Errorf("remain locks: %d", len(locks)))
		if err != nil {
			return nil, errors.Trace(err)
		}
		region, err1 := s.GetRegionCache().LocateKey(bo, locks[0].Key)
		if err1 != nil {
			return nil, errors.Trace(err1)
		}
		if !region.Contains(locks[len(locks)-1].Key) {
			// retry scan since the locks are not in the same region anymore.
			return nil, nil
		}
		resolvedLocation = region
	}
}
