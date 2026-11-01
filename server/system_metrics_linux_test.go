//go:build linux

package server

import (
	"math"
	"testing"
	"time"
)

func TestSystemMetricsMissingCgroupIsUnknownNotZero(t *testing.T) {
	s := &Server{}
	disk := metricValue{Available: true, UsedBytes: 10, TotalBytes: 100, Percent: 10}
	got := systemMetricsSnapshotFromSamples(s, time.Unix(100, 0), 0, 0, false, metricValue{}, disk, metricValue{})
	if !got.Supported || got.Scope != "system" || got.Container || got.CPU.Available || got.Memory.Available {
		t.Fatalf("missing cgroup measurements were presented as container readings: %+v", got)
	}
	if !got.Disk.Available || got.DataDisk.Available {
		t.Fatalf("missing data-disk measurement was replaced with a different filesystem: %+v", got)
	}
}

func TestSystemMetricsCPUWarmupAndInterruptedSample(t *testing.T) {
	s := &Server{}
	at := time.Unix(100, 0)
	memory := metricValue{Available: true, UsedBytes: 25, TotalBytes: 100, Percent: 25}
	sample := func(offset time.Duration, use uint64, ok bool) systemMetricsResponse {
		return systemMetricsSnapshotFromSamples(s, at.Add(offset), use, 2, ok, memory, metricValue{}, metricValue{})
	}
	first := sample(0, 0, true)
	if first.CPU.Available || first.Scope != "cgroup" || !first.Memory.Available {
		t.Fatalf("first CPU sample is not a percentage measurement: %+v", first)
	}
	second := sample(2*time.Second, 2_000_000, true)
	if !second.CPU.Available || second.CPU.Percent != 50 {
		t.Fatalf("CPU delta should be 50%%: %+v", second.CPU)
	}
	if got := sample(4*time.Second, 0, false); got.CPU.Available {
		t.Fatalf("failed read reported a CPU measurement: %+v", got.CPU)
	}
	if got := sample(6*time.Second, 4_000_000, true); got.CPU.Available {
		t.Fatalf("recovery reused a stale sampling window: %+v", got.CPU)
	}
	if got := sample(8*time.Second, 1, true); got.CPU.Available {
		t.Fatalf("reset usage counter reported a percentage: %+v", got.CPU)
	}
	if got := sample(10*time.Second, 1, true); !got.CPU.Available || got.CPU.Percent != 0 {
		t.Fatalf("two valid unchanged samples should report genuine zero usage: %+v", got.CPU)
	}
}

func TestSystemMetricsInvalidCapacitiesRemainUnknown(t *testing.T) {
	for _, capacity := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		s := &Server{}
		got := systemMetricsSnapshotFromSamples(s, time.Unix(100, 0), 1, capacity, true,
			metricValue{Available: true, TotalBytes: 0, Percent: 99}, metricValue{}, metricValue{})
		if got.CPU.Available || got.Memory.Available || got.Scope != "system" {
			t.Fatalf("invalid capacity reported a supported reading: %+v", got)
		}
	}
}
