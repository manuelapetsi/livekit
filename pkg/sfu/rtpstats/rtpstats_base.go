// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtpstats

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	cGapHistogramNumBins = 101
	cNumSequenceNumbers  = 65536
	cFirstSnapshotID     = 1

	cFirstPacketTimeAdjustWindow    = 2 * time.Minute
	cFirstPacketTimeAdjustThreshold = 15 * 1e9

	cSequenceNumberLargeJumpThreshold = 100
)

// -------------------------------------------------------

func RTPDriftToString(r *livekit.RTPDrift) string {
	if r == nil {
		return "-"
	}

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", r.StartTime.AsTime().Format(time.UnixDate), r.EndTime.AsTime().Format(time.UnixDate), r.Duration)
	str += fmt.Sprintf(", ts: %d|%d|%d", r.StartTimestamp, r.EndTimestamp, r.RtpClockTicks)
	str += fmt.Sprintf(", d: %d|%.2fms", r.DriftSamples, r.DriftMs)
	str += fmt.Sprintf(", cr: %.2f", r.ClockRate)
	return str
}

// -------------------------------------------------------

type RTPDeltaInfo struct {
	StartTime            time.Time
	EndTime              time.Time
	Packets              uint32
	Bytes                uint64
	HeaderBytes          uint64
	PacketsDuplicate     uint32
	BytesDuplicate       uint64
	HeaderBytesDuplicate uint64
	PacketsPadding       uint32
	BytesPadding         uint64
	HeaderBytesPadding   uint64
	PacketsLost          uint32
	PacketsMissing       uint32
	PacketsOutOfOrder    uint32
	Frames               uint32
	RttMax               uint32
	JitterMax            float64
	Nacks                uint32
	Plis                 uint32
	Firs                 uint32
}

type snapshot struct {
	isValid bool

	startTime time.Time

	extStartSN  uint64
	bytes       uint64
	headerBytes uint64

	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64

	packetsDuplicate     uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	frames uint32

	nacks uint32
	plis  uint32
	firs  uint32

	maxRtt    uint32
	maxJitter float64
}

// ------------------------------------------------------------------

type wrappedRTPDriftLogger struct {
	*livekit.RTPDrift
}

func (w wrappedRTPDriftLogger) MarshalLogObject(e zapcore.ObjectEncoder) error {
	rd := w.RTPDrift
	if rd == nil {
		return nil
	}

	e.AddTime("StartTime", rd.StartTime.AsTime())
	e.AddTime("EndTime", rd.EndTime.AsTime())
	e.AddFloat64("Duration", rd.Duration)
	e.AddUint64("StartTimestamp", rd.StartTimestamp)
	e.AddUint64("EndTimestamp", rd.EndTimestamp)
	e.AddUint64("RtpClockTicks", rd.RtpClockTicks)
	e.AddInt64("DriftSamples", rd.DriftSamples)
	e.AddFloat64("DriftMs", rd.DriftMs)
	e.AddFloat64("ClockRate", rd.ClockRate)
	return nil
}

// ------------------------------------------------------------------

type WrappedRTCPSenderReportStateLogger struct {
	*livekit.RTCPSenderReportState
}

func (w WrappedRTCPSenderReportStateLogger) MarshalLogObject(e zapcore.ObjectEncoder) error {
	rsrs := w.RTCPSenderReportState
	if rsrs == nil {
		return nil
	}

	e.AddUint32("RtpTimestamp", rsrs.RtpTimestamp)
	e.AddUint64("RtpTimestampExt", rsrs.RtpTimestampExt)
	e.AddTime("NtpTimestamp", mediatransportutil.NtpTime(rsrs.NtpTimestamp).Time())
	e.AddTime("At", time.Unix(0, rsrs.At))
	e.AddTime("AtAdjusted", time.Unix(0, rsrs.AtAdjusted))
	e.AddUint32("Packets", rsrs.Packets)
	e.AddUint64("Octets", rsrs.Octets)
	return nil
}

func RTCPSenderReportPropagationDelay(rsrs *livekit.RTCPSenderReportState, passThrough bool) time.Duration {
	if passThrough {
		return 0
	}

	return time.Unix(0, rsrs.AtAdjusted).Sub(mediatransportutil.NtpTime(rsrs.NtpTimestamp).Time())
}

// ------------------------------------------------------------------

type RTPStatsParams struct {
	ClockRate uint32
	Logger    logger.Logger
}

type rtpStatsBase struct {
	params RTPStatsParams
	logger logger.Logger

	lock sync.RWMutex

	initialized bool

	startTime time.Time
	endTime   time.Time

	firstTime           int64
	firstTimeAdjustment time.Duration
	highestTime         int64

	lastTransit            uint64
	lastJitterExtTimestamp uint64

	bytes                uint64
	headerBytes          uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64
	bytesPadding         uint64
	headerBytesPadding   uint64
	packetsDuplicate     uint64
	packetsPadding       uint64

	packetsOutOfOrder uint64

	packetsLost uint64

	frames uint32

	jitter    float64
	maxJitter float64

	gapHistogram [cGapHistogramNumBins]uint32

	nacks        uint32
	nackAcks     uint32
	nackMisses   uint32
	nackRepeated uint32

	plis    uint32
	lastPli time.Time

	layerLockPlis    uint32
	lastLayerLockPli time.Time

	firs    uint32
	lastFir time.Time

	keyFrames    uint32
	lastKeyFrame time.Time

	rtt    uint32
	maxRtt uint32

	srFirst  *livekit.RTCPSenderReportState
	srNewest *livekit.RTCPSenderReportState

	nextSnapshotID uint32
	snapshots      []snapshot
}

func newRTPStatsBase(params RTPStatsParams) *rtpStatsBase {
	return &rtpStatsBase{
		params:         params,
		logger:         params.Logger,
		nextSnapshotID: cFirstSnapshotID,
		snapshots:      make([]snapshot, 2),
	}
}

func (r *rtpStatsBase) seed(from *rtpStatsBase) bool {
	if from == nil || !from.initialized {
		return false
	}

	r.initialized = from.initialized

	r.startTime = from.startTime
	// do not clone endTime as a non-zero endTime indicates an ended object

	r.firstTime = from.firstTime
	r.highestTime = from.highestTime

	r.lastTransit = from.lastTransit
	r.lastJitterExtTimestamp = from.lastJitterExtTimestamp

	r.bytes = from.bytes
	r.headerBytes = from.headerBytes
	r.bytesDuplicate = from.bytesDuplicate
	r.headerBytesDuplicate = from.headerBytesDuplicate
	r.bytesPadding = from.bytesPadding
	r.headerBytesPadding = from.headerBytesPadding
	r.packetsDuplicate = from.packetsDuplicate
	r.packetsPadding = from.packetsPadding

	r.packetsOutOfOrder = from.packetsOutOfOrder

	r.packetsLost = from.packetsLost

	r.frames = from.frames

	r.jitter = from.jitter
	r.maxJitter = from.maxJitter

	r.gapHistogram = from.gapHistogram

	r.nacks = from.nacks
	r.nackAcks = from.nackAcks
	r.nackMisses = from.nackMisses
	r.nackRepeated = from.nackRepeated

	r.plis = from.plis
	r.lastPli = from.lastPli

	r.layerLockPlis = from.layerLockPlis
	r.lastLayerLockPli = from.lastLayerLockPli

	r.firs = from.firs
	r.lastFir = from.lastFir

	r.keyFrames = from.keyFrames
	r.lastKeyFrame = from.lastKeyFrame

	r.rtt = from.rtt
	r.maxRtt = from.maxRtt

	r.srFirst = utils.CloneProto(from.srFirst)
	r.srNewest = utils.CloneProto(from.srNewest)

	r.nextSnapshotID = from.nextSnapshotID
	r.snapshots = make([]snapshot, cap(from.snapshots))
	copy(r.snapshots, from.snapshots)
	return true
}

func (r *rtpStatsBase) SetLogger(logger logger.Logger) {
	r.logger = logger
}

func (r *rtpStatsBase) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.endTime = time.Now()
}

func (r *rtpStatsBase) newSnapshotID(extStartSN uint64) uint32 {
	id := r.nextSnapshotID
	r.nextSnapshotID++

	if cap(r.snapshots) < int(r.nextSnapshotID-cFirstSnapshotID) {
		snapshots := make([]snapshot, r.nextSnapshotID-cFirstSnapshotID)
		copy(snapshots, r.snapshots)
		r.snapshots = snapshots
	}

	if r.initialized {
		r.snapshots[id-cFirstSnapshotID] = r.initSnapshot(time.Now(), extStartSN)
	}
	return id
}

func (r *rtpStatsBase) IsActive() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.initialized && r.endTime.IsZero()
}

func (r *rtpStatsBase) UpdateNack(nackCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nacks += nackCount
}

func (r *rtpStatsBase) UpdateNackProcessed(nackAckCount uint32, nackMissCount uint32, nackRepeatedCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nackAcks += nackAckCount
	r.nackMisses += nackMissCount
	r.nackRepeated += nackRepeatedCount
}

func (r *rtpStatsBase) CheckAndUpdatePli(throttle int64, force bool) bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() || (!force && time.Now().UnixNano()-r.lastPli.UnixNano() < throttle) {
		return false
	}
	r.updatePliLocked(1)
	r.updatePliTimeLocked()
	return true
}

func (r *rtpStatsBase) UpdatePliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
	r.updatePliTimeLocked()
}

func (r *rtpStatsBase) UpdatePli(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
}

func (r *rtpStatsBase) updatePliLocked(pliCount uint32) {
	r.plis += pliCount
}

func (r *rtpStatsBase) UpdatePliTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliTimeLocked()
}

func (r *rtpStatsBase) updatePliTimeLocked() {
	r.lastPli = time.Now()
}

func (r *rtpStatsBase) LastPli() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastPli
}

func (r *rtpStatsBase) UpdateLayerLockPliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.layerLockPlis += pliCount
	r.lastLayerLockPli = time.Now()
}

func (r *rtpStatsBase) UpdateFir(firCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.firs += firCount
}

func (r *rtpStatsBase) UpdateFirTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.lastFir = time.Now()
}

func (r *rtpStatsBase) UpdateKeyFrame(kfCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.keyFrames += kfCount
	r.lastKeyFrame = time.Now()
}

func (r *rtpStatsBase) UpdateRtt(rtt uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.rtt = rtt
	if rtt > r.maxRtt {
		r.maxRtt = rtt
	}

	for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
		s := &r.snapshots[i]
		if rtt > s.maxRtt {
			s.maxRtt = rtt
		}
	}
}

func (r *rtpStatsBase) GetRtt() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtt
}

func (r *rtpStatsBase) maybeAdjustFirstPacketTime(srData *livekit.RTCPSenderReportState, tsOffset uint64, extStartTS uint64) (err error, loggingFields []interface{}) {
	if time.Since(r.startTime) > cFirstPacketTimeAdjustWindow {
		return
	}

	// for some time after the start, adjust time of first packet.
	// Helps improve accuracy of expected timestamp calculation.
	// Adjusting only one way, i. e. if the first sample experienced
	// abnormal delay (maybe due to pacing or maybe due to queuing
	// in some network element along the way), push back first time
	// to an earlier instance.
	timeSinceReceive := time.Since(time.Unix(0, srData.AtAdjusted))
	extNowTS := srData.RtpTimestampExt - tsOffset + uint64(timeSinceReceive.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	samplesDiff := int64(extNowTS - extStartTS)
	if samplesDiff < 0 {
		// out-of-order, skip
		return
	}

	samplesDuration := time.Duration(float64(samplesDiff) / float64(r.params.ClockRate) * float64(time.Second))
	timeSinceFirst := time.Since(time.Unix(0, r.firstTime))
	now := r.firstTime + timeSinceFirst.Nanoseconds()
	firstTime := now - samplesDuration.Nanoseconds()

	getFields := func() []interface{} {
		return []interface{}{
			"startTime", r.startTime.String(),
			"nowTime", time.Unix(0, now).String(),
			"before", time.Unix(0, r.firstTime).String(),
			"after", time.Unix(0, firstTime).String(),
			"adjustment", time.Duration(r.firstTime - firstTime).String(),
			"extNowTS", extNowTS,
			"extStartTS", extStartTS,
			"srData", WrappedRTCPSenderReportStateLogger{srData},
			"tsOffset", tsOffset,
			"timeSinceReceive", timeSinceReceive.String(),
			"timeSinceFirst", timeSinceFirst.String(),
			"samplesDiff", samplesDiff,
			"samplesDuration", samplesDuration,
		}
	}

	if firstTime < r.firstTime {
		if r.firstTime-firstTime > cFirstPacketTimeAdjustThreshold {
			err = errors.New("adjusting first packet time, too big, ignoring")
			loggingFields = getFields()
		} else {
			r.logger.Debugw("adjusting first packet time", getFields()...)
			r.firstTimeAdjustment += time.Duration(r.firstTime - firstTime)
			r.firstTime = firstTime
		}
	}
	return
}

func (r *rtpStatsBase) getTotalPacketsPrimary(extStartSN, extHighestSN uint64) uint64 {
	packetsExpected := extHighestSN - extStartSN + 1
	if r.packetsLost > packetsExpected {
		// should not happen
		return 0
	}

	packetsSeen := packetsExpected - r.packetsLost
	if r.packetsPadding > packetsSeen {
		return 0
	}

	return packetsSeen - r.packetsPadding
}

func (r *rtpStatsBase) deltaInfo(snapshotID uint32, extStartSN uint64, extHighestSN uint64) (deltaInfo *RTPDeltaInfo, err error, loggingFields []interface{}) {
	then, now := r.getAndResetSnapshot(snapshotID, extStartSN, extHighestSN)
	if now == nil || then == nil {
		return
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSN - then.extStartSN
	if then.extStartSN > extHighestSN {
		packetsExpected = 0
	}
	if packetsExpected > cNumSequenceNumbers {
		loggingFields = []interface{}{
			"startSN", then.extStartSN,
			"endSN", now.extStartSN,
			"packetsExpected", packetsExpected,
			"startTime", startTime,
			"endTime", endTime,
			"duration", endTime.Sub(startTime).String(),
		}
		err = errors.New("too many packets expected in delta")
		return
	}
	if packetsExpected == 0 {
		deltaInfo = &RTPDeltaInfo{
			StartTime: startTime,
			EndTime:   endTime,
		}
		return
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}

	// padding packets delta could be higher than expected due to out-of-order padding packets
	packetsPadding := now.packetsPadding - then.packetsPadding
	if packetsExpected < packetsPadding {
		loggingFields = []interface{}{
			"packetsExpected", packetsExpected,
			"packetsPadding", packetsPadding,
			"packetsLost", packetsLost,
			"startSequenceNumber", then.extStartSN,
			"endSequenceNumber", now.extStartSN - 1,
		}
		err = errors.New("padding packets more than expected")
		packetsExpected = 0
	} else {
		packetsExpected -= packetsPadding
	}

	deltaInfo = &RTPDeltaInfo{
		StartTime:            startTime,
		EndTime:              endTime,
		Packets:              uint32(packetsExpected),
		Bytes:                now.bytes - then.bytes,
		HeaderBytes:          now.headerBytes - then.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(packetsPadding),
		BytesPadding:         now.bytesPadding - then.bytesPadding,
		HeaderBytesPadding:   now.headerBytesPadding - then.headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsOutOfOrder:    uint32(now.packetsOutOfOrder - then.packetsOutOfOrder),
		Frames:               now.frames - then.frames,
		RttMax:               then.maxRtt,
		JitterMax:            then.maxJitter / float64(r.params.ClockRate) * 1e6,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
	return
}

func (r *rtpStatsBase) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	e.AddTime("startTime", r.startTime)
	e.AddTime("endTime", r.endTime)
	e.AddTime("firstTime", time.Unix(0, r.firstTime))
	e.AddDuration("firstTimeAdjustment", r.firstTimeAdjustment)
	e.AddTime("highestTime", time.Unix(0, r.highestTime))

	e.AddUint64("bytes", r.bytes)
	e.AddUint64("headerBytes", r.headerBytes)

	e.AddUint64("packetsDuplicate", r.packetsDuplicate)
	e.AddUint64("bytesDuplicate", r.bytesDuplicate)
	e.AddUint64("headerBytesDuplicate", r.headerBytesDuplicate)

	e.AddUint64("packetsPadding", r.packetsPadding)
	e.AddUint64("bytesPadding", r.bytesPadding)
	e.AddUint64("headerBytesPadding", r.headerBytesPadding)

	e.AddUint64("packetsOutOfOrder", r.packetsOutOfOrder)

	e.AddUint64("packetsLost", r.packetsLost)

	e.AddUint32("frames", r.frames)

	e.AddFloat64("jitter", r.jitter)
	e.AddFloat64("maxJitter", r.maxJitter)

	hasLoss := false
	first := true
	str := "["
	for burst, count := range r.gapHistogram {
		if count == 0 {
			continue
		}

		hasLoss = true

		if !first {
			str += ", "
		}
		first = false
		str += fmt.Sprintf("%d:%d", burst+1, count)
	}
	str += "]"
	if hasLoss {
		e.AddString("gapHistogram", str)
	}

	e.AddUint32("nacks", r.nacks)
	e.AddUint32("nackAcks", r.nackAcks)
	e.AddUint32("nackMisses", r.nackMisses)
	e.AddUint32("nackRepeated", r.nackRepeated)

	e.AddUint32("plis", r.plis)
	e.AddTime("lastPli", r.lastPli)

	e.AddUint32("layerLockPlis", r.layerLockPlis)
	e.AddTime("lastLayerLockPli", r.lastLayerLockPli)

	e.AddUint32("firs", r.firs)
	e.AddTime("lastFir", r.lastFir)

	e.AddUint32("keyFrames", r.keyFrames)
	e.AddTime("lastKeyFrame", r.lastKeyFrame)

	e.AddUint32("rtt", r.rtt)
	e.AddUint32("maxRtt", r.maxRtt)

	e.AddObject("srFirst", WrappedRTCPSenderReportStateLogger{r.srFirst})
	e.AddObject("srNewest", WrappedRTCPSenderReportStateLogger{r.srNewest})
	return nil
}

func (r *rtpStatsBase) toString(
	extStartSN, extHighestSN, extStartTS, extHighestTS uint64,
	packetsLost uint64,
	jitter, maxJitter float64,
) string {
	p := r.toProto(
		extStartSN, extHighestSN, extStartTS, extHighestTS,
		packetsLost,
		jitter, maxJitter,
	)
	if p == nil {
		return ""
	}

	expectedPackets := extHighestSN - extStartSN + 1
	expectedPacketRate := float64(expectedPackets) / p.Duration

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", p.StartTime.AsTime().Format(time.UnixDate), p.EndTime.AsTime().Format(time.UnixDate), p.Duration)

	str += fmt.Sprintf(", sn: %d|%d", extStartSN, extHighestSN)
	str += fmt.Sprintf(", ep: %d|%.2f/s", expectedPackets, expectedPacketRate)

	str += fmt.Sprintf(", p: %d|%.2f/s", p.Packets, p.PacketRate)
	str += fmt.Sprintf(", l: %d|%.1f/s|%.2f%%", p.PacketsLost, p.PacketLossRate, p.PacketLossPercentage)
	str += fmt.Sprintf(", b: %d|%.1fbps|%d", p.Bytes, p.Bitrate, p.HeaderBytes)
	str += fmt.Sprintf(", f: %d|%.1f/s / %d|%+v", p.Frames, p.FrameRate, p.KeyFrames, p.LastKeyFrame.AsTime().Format(time.UnixDate))

	str += fmt.Sprintf(", d: %d|%.2f/s", p.PacketsDuplicate, p.PacketDuplicateRate)
	str += fmt.Sprintf(", bd: %d|%.1fbps|%d", p.BytesDuplicate, p.BitrateDuplicate, p.HeaderBytesDuplicate)

	str += fmt.Sprintf(", pp: %d|%.2f/s", p.PacketsPadding, p.PacketPaddingRate)
	str += fmt.Sprintf(", bp: %d|%.1fbps|%d", p.BytesPadding, p.BitratePadding, p.HeaderBytesPadding)

	str += fmt.Sprintf(", o: %d", p.PacketsOutOfOrder)

	str += fmt.Sprintf(", c: %d, j: %d(%.1fus)|%d(%.1fus)", r.params.ClockRate, uint32(jitter), p.JitterCurrent, uint32(maxJitter), p.JitterMax)

	if len(p.GapHistogram) != 0 {
		first := true
		str += ", gh:["
		for burst, count := range p.GapHistogram {
			if !first {
				str += ", "
			}
			first = false
			str += fmt.Sprintf("%d:%d", burst, count)
		}
		str += "]"
	}

	str += ", n:"
	str += fmt.Sprintf("%d|%d|%d|%d", p.Nacks, p.NackAcks, p.NackMisses, p.NackRepeated)

	str += ", pli:"
	str += fmt.Sprintf("%d|%+v / %d|%+v",
		p.Plis, p.LastPli.AsTime().Format(time.UnixDate),
		p.LayerLockPlis, p.LastLayerLockPli.AsTime().Format(time.UnixDate),
	)

	str += ", fir:"
	str += fmt.Sprintf("%d|%+v", p.Firs, p.LastFir.AsTime().Format(time.UnixDate))

	str += ", rtt(ms):"
	str += fmt.Sprintf("%d|%d", p.RttCurrent, p.RttMax)

	str += fmt.Sprintf(", pd: %s, nrd: %s, rxrd: %s, rbrd: %s",
		RTPDriftToString(p.PacketDrift),
		RTPDriftToString(p.NtpReportDrift),
		RTPDriftToString(p.ReceivedReportDrift),
		RTPDriftToString(p.RebasedReportDrift),
	)
	return str
}

func (r *rtpStatsBase) toProto(
	extStartSN, extHighestSN, extStartTS, extHighestTS uint64,
	packetsLost uint64,
	jitter, maxJitter float64,
) *livekit.RTPStats {
	if r.startTime.IsZero() {
		return nil
	}

	endTime := r.endTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(r.startTime).Seconds()
	if elapsed == 0.0 {
		return nil
	}

	packets := r.getTotalPacketsPrimary(extStartSN, extHighestSN)
	packetRate := float64(packets) / elapsed
	bitrate := float64(r.bytes) * 8.0 / elapsed

	frameRate := float64(r.frames) / elapsed

	packetsExpected := extHighestSN - extStartSN + 1
	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / float32(packetsExpected) * 100.0

	packetDuplicateRate := float64(r.packetsDuplicate) / elapsed
	bitrateDuplicate := float64(r.bytesDuplicate) * 8.0 / elapsed

	packetPaddingRate := float64(r.packetsPadding) / elapsed
	bitratePadding := float64(r.bytesPadding) * 8.0 / elapsed

	jitterTime := jitter / float64(r.params.ClockRate) * 1e6
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	packetDrift, ntpReportDrift, receivedReportDrift, rebasedReportDrift := r.getDrift(extStartTS, extHighestTS)

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(r.startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              uint32(packets),
		PacketRate:           packetRate,
		Bytes:                r.bytes,
		HeaderBytes:          r.headerBytes,
		Bitrate:              bitrate,
		PacketsLost:          uint32(packetsLost),
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     uint32(r.packetsDuplicate),
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       r.bytesDuplicate,
		HeaderBytesDuplicate: r.headerBytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       uint32(r.packetsPadding),
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         r.bytesPadding,
		HeaderBytesPadding:   r.headerBytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    uint32(r.packetsOutOfOrder),
		Frames:               r.frames,
		FrameRate:            frameRate,
		KeyFrames:            r.keyFrames,
		LastKeyFrame:         timestamppb.New(r.lastKeyFrame),
		JitterCurrent:        jitterTime,
		JitterMax:            maxJitterTime,
		Nacks:                r.nacks,
		NackAcks:             r.nackAcks,
		NackMisses:           r.nackMisses,
		NackRepeated:         r.nackRepeated,
		Plis:                 r.plis,
		LastPli:              timestamppb.New(r.lastPli),
		LayerLockPlis:        r.layerLockPlis,
		LastLayerLockPli:     timestamppb.New(r.lastLayerLockPli),
		Firs:                 r.firs,
		LastFir:              timestamppb.New(r.lastFir),
		RttCurrent:           r.rtt,
		RttMax:               r.maxRtt,
		PacketDrift:          packetDrift,
		NtpReportDrift:       ntpReportDrift,
		RebasedReportDrift:   rebasedReportDrift,
		ReceivedReportDrift:  receivedReportDrift,
	}

	gapsPresent := false
	for i := 0; i < len(r.gapHistogram); i++ {
		if r.gapHistogram[i] == 0 {
			continue
		}

		gapsPresent = true
		break
	}

	if gapsPresent {
		p.GapHistogram = make(map[int32]uint32, len(r.gapHistogram))
		for i := 0; i < len(r.gapHistogram); i++ {
			if r.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = r.gapHistogram[i]
		}
	}

	return p
}

func (r *rtpStatsBase) updateJitter(ets uint64, packetTime int64) float64 {
	// Do not update jitter on multiple packets of same frame.
	// All packets of a frame have the same time stamp.
	// NOTE: This does not protect against using more than one packet of the same frame
	//       if packets arrive out-of-order. For example,
	//          p1f1 -> p1f2 -> p2f1
	//       In this case, p2f1 (packet 2, frame 1) will still be used in jitter calculation
	//       although it is the second packet of a frame because of out-of-order receival.
	if r.lastJitterExtTimestamp != ets {
		timeSinceFirst := packetTime - r.firstTime
		packetTimeRTP := uint64(timeSinceFirst * int64(r.params.ClockRate) / 1e9)
		transit := packetTimeRTP - ets

		if r.lastTransit != 0 {
			d := int64(transit - r.lastTransit)
			if d < 0 {
				d = -d
			}
			r.jitter += (float64(d) - r.jitter) / 16
			if r.jitter > r.maxJitter {
				r.maxJitter = r.jitter
			}

			for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
				s := &r.snapshots[i]
				if r.jitter > s.maxJitter {
					s.maxJitter = r.jitter
				}
			}
		}

		r.lastTransit = transit
		r.lastJitterExtTimestamp = ets
	}
	return r.jitter
}

func (r *rtpStatsBase) getAndResetSnapshot(snapshotID uint32, extStartSN uint64, extHighestSN uint64) (*snapshot, *snapshot) {
	if !r.initialized {
		return nil, nil
	}

	idx := snapshotID - cFirstSnapshotID
	then := r.snapshots[idx]
	if !then.isValid {
		then = r.initSnapshot(r.startTime, extStartSN)
		r.snapshots[idx] = then
	}

	// snapshot now
	now := r.getSnapshot(time.Now(), extHighestSN+1)
	r.snapshots[idx] = now
	return &then, &now
}

func (r *rtpStatsBase) getDrift(extStartTS, extHighestTS uint64) (packetDrift *livekit.RTPDrift, ntpReportDrift *livekit.RTPDrift, receivedReportDrift *livekit.RTPDrift, rebasedReportDrift *livekit.RTPDrift) {
	if r.firstTime != 0 {
		elapsed := r.highestTime - r.firstTime
		rtpClockTicks := extHighestTS - extStartTS
		driftSamples := int64(rtpClockTicks - uint64(elapsed*int64(r.params.ClockRate)/1e9))
		if elapsed > 0 {
			elapsedSeconds := time.Duration(elapsed).Seconds()
			packetDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.firstTime)),
				EndTime:        timestamppb.New(time.Unix(0, r.highestTime)),
				Duration:       elapsedSeconds,
				StartTimestamp: extStartTS,
				EndTimestamp:   extHighestTS,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsedSeconds,
			}
		}
	}

	if r.srFirst != nil && r.srNewest != nil && r.srFirst.RtpTimestamp != r.srNewest.RtpTimestamp {
		rtpClockTicks := r.srNewest.RtpTimestampExt - r.srFirst.RtpTimestampExt

		elapsed := mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time().Sub(mediatransportutil.NtpTime(r.srFirst.NtpTimestamp).Time())
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			ntpReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(mediatransportutil.NtpTime(r.srFirst.NtpTimestamp).Time()),
				EndTime:        timestamppb.New(mediatransportutil.NtpTime(r.srNewest.NtpTimestamp).Time()),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}

		elapsed = time.Duration(r.srNewest.At - r.srFirst.At)
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			receivedReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.srFirst.At)),
				EndTime:        timestamppb.New(time.Unix(0, r.srNewest.At)),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}

		elapsed = time.Duration(r.srNewest.AtAdjusted - r.srFirst.AtAdjusted)
		if elapsed.Seconds() > 0.0 {
			driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
			rebasedReportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(time.Unix(0, r.srFirst.AtAdjusted)),
				EndTime:        timestamppb.New(time.Unix(0, r.srNewest.AtAdjusted)),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RtpTimestampExt,
				EndTimestamp:   r.srNewest.RtpTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}
	}
	return
}

func (r *rtpStatsBase) updateGapHistogram(gap int) {
	if gap < 2 {
		return
	}

	missing := gap - 1
	if missing > len(r.gapHistogram) {
		r.gapHistogram[len(r.gapHistogram)-1]++
	} else {
		r.gapHistogram[missing-1]++
	}
}

func (r *rtpStatsBase) initSnapshot(startTime time.Time, extStartSN uint64) snapshot {
	return snapshot{
		isValid:    true,
		startTime:  startTime,
		extStartSN: extStartSN,
	}
}

func (r *rtpStatsBase) getSnapshot(startTime time.Time, extStartSN uint64) snapshot {
	return snapshot{
		isValid:              true,
		startTime:            startTime,
		extStartSN:           extStartSN,
		bytes:                r.bytes,
		headerBytes:          r.headerBytes,
		packetsPadding:       r.packetsPadding,
		bytesPadding:         r.bytesPadding,
		headerBytesPadding:   r.headerBytesPadding,
		packetsDuplicate:     r.packetsDuplicate,
		bytesDuplicate:       r.bytesDuplicate,
		headerBytesDuplicate: r.headerBytesDuplicate,
		packetsLost:          r.packetsLost,
		packetsOutOfOrder:    r.packetsOutOfOrder,
		frames:               r.frames,
		nacks:                r.nacks,
		plis:                 r.plis,
		firs:                 r.firs,
		maxRtt:               r.rtt,
		maxJitter:            r.jitter,
	}
}

// ----------------------------------

func AggregateRTPStats(statsList []*livekit.RTPStats) *livekit.RTPStats {
	return utils.AggregateRTPStats(statsList, cGapHistogramNumBins)
}

func AggregateRTPDeltaInfo(deltaInfoList []*RTPDeltaInfo) *RTPDeltaInfo {
	if len(deltaInfoList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	headerBytes := uint64(0)

	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	headerBytesDuplicate := uint64(0)

	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	headerBytesPadding := uint64(0)

	packetsLost := uint32(0)
	packetsMissing := uint32(0)
	packetsOutOfOrder := uint32(0)

	frames := uint32(0)

	maxRtt := uint32(0)
	maxJitter := float64(0)

	nacks := uint32(0)
	plis := uint32(0)
	firs := uint32(0)

	for _, deltaInfo := range deltaInfoList {
		if deltaInfo == nil {
			continue
		}

		if startTime.IsZero() || startTime.After(deltaInfo.StartTime) {
			startTime = deltaInfo.StartTime
		}

		if endTime.IsZero() || endTime.Before(deltaInfo.EndTime) {
			endTime = deltaInfo.EndTime
		}

		packets += deltaInfo.Packets
		bytes += deltaInfo.Bytes
		headerBytes += deltaInfo.HeaderBytes

		packetsDuplicate += deltaInfo.PacketsDuplicate
		bytesDuplicate += deltaInfo.BytesDuplicate
		headerBytesDuplicate += deltaInfo.HeaderBytesDuplicate

		packetsPadding += deltaInfo.PacketsPadding
		bytesPadding += deltaInfo.BytesPadding
		headerBytesPadding += deltaInfo.HeaderBytesPadding

		packetsLost += deltaInfo.PacketsLost
		packetsMissing += deltaInfo.PacketsMissing
		packetsOutOfOrder += deltaInfo.PacketsOutOfOrder

		frames += deltaInfo.Frames

		if deltaInfo.RttMax > maxRtt {
			maxRtt = deltaInfo.RttMax
		}

		if deltaInfo.JitterMax > maxJitter {
			maxJitter = deltaInfo.JitterMax
		}

		nacks += deltaInfo.Nacks
		plis += deltaInfo.Plis
		firs += deltaInfo.Firs
	}
	if startTime.IsZero() || endTime.IsZero() {
		return nil
	}

	return &RTPDeltaInfo{
		StartTime:            startTime,
		EndTime:              endTime,
		Packets:              packets,
		Bytes:                bytes,
		HeaderBytes:          headerBytes,
		PacketsDuplicate:     packetsDuplicate,
		BytesDuplicate:       bytesDuplicate,
		HeaderBytesDuplicate: headerBytesDuplicate,
		PacketsPadding:       packetsPadding,
		BytesPadding:         bytesPadding,
		HeaderBytesPadding:   headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsMissing:       packetsMissing,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		RttMax:               maxRtt,
		JitterMax:            maxJitter,
		Nacks:                nacks,
		Plis:                 plis,
		Firs:                 firs,
	}
}

// -------------------------------------------------------------------
