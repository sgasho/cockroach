// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package closedts

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/closedts/ctpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
)

const (
	DefaultMaxNetworkRTT             = 150 * time.Millisecond
	closedTimestampPolicyBucketWidth = 20 * time.Millisecond
)

// computeLeadTimeForGlobalReads calculates how far ahead of the current time a
// node should publish closed timestamps to ensure that followers can serve
// global reads. It accounts for network latency, clock offset, and both Raft
// and side-transport propagation delays.
func computeLeadTimeForGlobalReads(
	networkRTT time.Duration, maxClockOffset time.Duration, sideTransportCloseInterval time.Duration,
) time.Duration {
	// The LEAD_FOR_GLOBAL_READS calculation is more complex. Instead of the
	// policy defining an offset from the publisher's perspective, the
	// policy defines a goal from the consumer's perspective - the goal
	// being that present time reads (with a possible uncertainty interval)
	// can be served from all followers. To accomplish this, we must work
	// backwards to establish a lead time to publish closed timestamps at.
	//
	// The calculation looks something like the following:
	//
	//  # This should be sufficient for any present-time transaction,
	//  # because its global uncertainty limit should be <= this time.
	//  # For more, see (*Transaction).RequiredFrontier.
	//  closed_ts_at_follower = now + max_offset
	//
	//  # The sender must account for the time it takes to propagate a
	//  # closed timestamp update to its followers.
	//  closed_ts_at_sender = closed_ts_at_follower + propagation_time
	//
	//  # Closed timestamps propagate in two ways. Both need to make it to
	//  # followers in time.
	//  propagation_time = max(raft_propagation_time, side_propagation_time)
	//
	//  # Raft propagation takes 3 network hops to go from a leader proposing
	//  # a write (with a closed timestamp update) to the write being applied.
	//  # 1. leader sends MsgProp with entry
	//  # 2. followers send MsgPropResp with vote
	//  # 3. leader sends MsgProp with higher commit index
	//  #
	//  # We also add on a small bit of overhead for request evaluation, log
	//  # sync, and state machine apply latency.
	//  raft_propagation_time = max_network_rtt * 1.5 + raft_overhead
	//
	//  # Side-transport propagation takes 1 network hop, as there is no voting.
	//  # However, it is delayed by the full side_transport_close_interval in
	//  # the worst-case.
	//  side_propagation_time = max_network_rtt * 0.5 + side_transport_close_interval
	//
	//  # Combine, we get the following result
	//  closed_ts_at_sender = now + max_offset + max(
	//    max_network_rtt * 1.5 + raft_overhead,
	//    max_network_rtt * 0.5 + side_transport_close_interval,
	//  )
	//
	// By default, this leads to a closed timestamp target that leads the
	// senders current clock by 800ms.
	//
	// NOTE: this calculation takes into consideration maximum clock skew as
	// it relates to a transaction's uncertainty interval, but it does not
	// take into consideration "effective" clock skew as it relates to a
	// follower replica having a faster clock than a leaseholder and
	// therefore needing the leaseholder to publish even further into the
	// future. Since the effect of getting this wrong is reduced performance
	// (i.e. missed follower reads) and not a correctness violation (i.e.
	// stale reads), we can be less strict here. We also expect that even
	// when two nodes have skewed physical clocks, the "stability" property
	// of HLC propagation when nodes are communicating should reduce the
	// effective HLC clock skew.
	// See raft_propagation_time.
	const raftTransportOverhead = 20 * time.Millisecond
	raftTransportPropTime := (networkRTT*3)/2 + raftTransportOverhead

	// See side_propagation_time.
	sideTransportPropTime := networkRTT/2 + sideTransportCloseInterval

	// See propagation_time.
	maxTransportPropTime := max(sideTransportPropTime, raftTransportPropTime)

	// Include a small amount of extra margin to smooth out temporary
	// network blips or anything else that slows down closed timestamp
	// propagation momentarily.
	const bufferTime = 25 * time.Millisecond
	return maxTransportPropTime + maxClockOffset + bufferTime
}

// TargetForPolicy returns the target closed timestamp for a range with the
// given policy.
func TargetForPolicy(
	now hlc.ClockTimestamp,
	maxClockOffset time.Duration,
	lagTargetDuration time.Duration,
	leadTargetOverride time.Duration,
	sideTransportCloseInterval time.Duration,
	policy ctpb.RangeClosedTimestampPolicy,
) hlc.Timestamp {
	var targetOffsetTime time.Duration
	switch {
	case policy == ctpb.LAG_BY_CLUSTER_SETTING:
		// Simple calculation: lag now by desired duration.
		targetOffsetTime = -lagTargetDuration
	case policy >= ctpb.LEAD_FOR_GLOBAL_READS_WITH_NO_LATENCY_INFO &&
		policy <= ctpb.LEAD_FOR_GLOBAL_READS_LATENCY_EQUAL_OR_GREATER_THAN_300MS:
		// Override entirely with cluster setting, if necessary.
		if leadTargetOverride != 0 {
			targetOffsetTime = leadTargetOverride
			break
		}
		targetOffsetTime = computeLeadTimeForGlobalReads(computeNetworkRTTBasedOnPolicy(policy),
			maxClockOffset, sideTransportCloseInterval)
	default:
		panic("unexpected RangeClosedTimestampPolicy")
	}
	res := now.ToTimestamp().Add(targetOffsetTime.Nanoseconds(), 0)
	// We truncate the logical part in order to save a few bytes over the network,
	// and also because arithmetic with logical timestamp doesn't make much sense.
	res.Logical = 0
	return res
}
