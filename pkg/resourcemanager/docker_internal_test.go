package resourcemanager

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mkStatsJSON(cur, pre types.CPUStats, mem types.MemoryStats) types.StatsJSON {
	var s types.StatsJSON
	s.CPUStats = cur
	s.PreCPUStats = pre
	s.MemoryStats = mem
	return s
}

func TestDockerContainerStats(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 10, 30, 0, 0, time.UTC)
	wantTime := metav1.NewTime(now)

	tests := []struct {
		name string
		in   types.StatsJSON

		wantUsageCoreNanoSeconds uint64
		// wantNanoCoresNil true asserts UsageNanoCores pointer is nil.
		wantNanoCoresNil bool
		wantNanoCores    uint64

		wantUsageBytes uint64
		wantWorkingSet uint64
		wantRSS        uint64
	}{
		{
			// cgroup v2 host shape: OnlineCPUs set, no PercpuUsage, anon + inactive_file present, no rss/cache key.
			name: "CgroupV2_HappyPath",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{
					Usage: 300_000_000,
					Stats: map[string]uint64{
						"anon":          120_000_000,
						"file":          150_000_000,
						"inactive_file": 50_000_000,
					},
				},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			// cpuDelta=1e9, systemDelta=2e9, onlineCPUs=2 -> 1e9/2e9*2*1e9 = 1e9
			wantNanoCores:  1_000_000_000,
			wantUsageBytes: 300_000_000,
			wantWorkingSet: 250_000_000, // Usage - inactive_file
			wantRSS:        120_000_000, // anon
		},
		{
			// cgroup v1 host shape: OnlineCPUs 0 -> fall back to len(PercpuUsage)=4; rss + cache + total_inactive_file.
			name: "CgroupV1_HappyPath",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage: types.CPUUsage{
						TotalUsage:  8_000_000_000,
						PercpuUsage: []uint64{1, 2, 3, 4},
					},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  0,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 6_000_000_000},
					SystemUsage: 96_000_000_000,
				},
				types.MemoryStats{
					Usage: 500_000_000,
					Stats: map[string]uint64{
						"rss":                 200_000_000,
						"cache":               90_000_000,
						"total_inactive_file": 70_000_000,
					},
				},
			),
			wantUsageCoreNanoSeconds: 8_000_000_000,
			// cpuDelta=2e9, systemDelta=4e9, onlineCPUs=4 -> 2e9/4e9*4*1e9 = 2e9
			wantNanoCores:  2_000_000_000,
			wantUsageBytes: 500_000_000,
			wantWorkingSet: 430_000_000, // Usage - total_inactive_file
			wantRSS:        200_000_000, // rss
		},
		{
			// OnlineCPUs (8) preferred over len(PercpuUsage)=4.
			name: "OnlineCPUsPreferredOverPercpu",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage: types.CPUUsage{
						TotalUsage:  3_000_000_000,
						PercpuUsage: []uint64{1, 2, 3, 4},
					},
					SystemUsage: 50_000_000_000,
					OnlineCPUs:  8,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 2_000_000_000},
					SystemUsage: 48_000_000_000,
				},
				types.MemoryStats{
					Usage: 100,
					Stats: map[string]uint64{"inactive_file": 10, "anon": 30},
				},
			),
			wantUsageCoreNanoSeconds: 3_000_000_000,
			// cpuDelta=1e9, systemDelta=2e9, onlineCPUs=8 -> 1e9/2e9*8*1e9 = 4e9
			wantNanoCores:  4_000_000_000,
			wantUsageBytes: 100,
			wantWorkingSet: 90,
			wantRSS:        30,
		},
		{
			// systemDelta == 0 -> nanocores nil.
			name: "NanoCoresNilWhenSystemDeltaZero",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 100_000_000_000,
				},
				types.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 10, "anon": 5}},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCoresNil:         true,
			wantUsageBytes:           100,
			wantWorkingSet:           90,
			wantRSS:                  5,
		},
		{
			// Idle: cpuDelta==0 with systemDelta>0 and onlineCPUs>0 -> nanocores non-nil 0.
			// Docker CLI shows 0.00%, cadvisor/kubelet report 0; nil would look like missing data.
			name: "NanoCoresZeroWhenIdle",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 10, "anon": 5}},
			),
			wantUsageCoreNanoSeconds: 4_000_000_000,
			wantNanoCores:            0,
			wantUsageBytes:           100,
			wantWorkingSet:           90,
			wantRSS:                  5,
		},
		{
			// CPU counter reset on container restart: cur TotalUsage < pre -> nanocores nil.
			// uint64 subtraction would wrap to a huge value and pass a naive cpuDelta>0 guard.
			name: "NanoCoresNilWhenCPUCounterReset",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 1_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 10, "anon": 5}},
			),
			wantUsageCoreNanoSeconds: 1_000_000_000,
			wantNanoCoresNil:         true,
			wantUsageBytes:           100,
			wantWorkingSet:           90,
			wantRSS:                  5,
		},
		{
			// System counter reset: cur SystemUsage < pre -> systemDelta not > 0 -> nanocores nil.
			name: "NanoCoresNilWhenSystemCounterReset",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 98_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 100_000_000_000,
				},
				types.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 10, "anon": 5}},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCoresNil:         true,
			wantUsageBytes:           100,
			wantWorkingSet:           90,
			wantRSS:                  5,
		},
		{
			// OnlineCPUs 0 AND PercpuUsage empty -> onlineCPUs 0 -> nanocores nil.
			name: "NanoCoresNilWhenNoCPUCount",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  0,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 10, "anon": 5}},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCoresNil:         true,
			wantUsageBytes:           100,
			wantWorkingSet:           90,
			wantRSS:                  5,
		},
		{
			// inactive_file > Usage -> WorkingSet clamps to 0 (cadvisor underflow rule).
			name: "WorkingSetUnderflowClampsZero",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{
					Usage: 100,
					Stats: map[string]uint64{"inactive_file": 500, "anon": 40},
				},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCores:            1_000_000_000,
			wantUsageBytes:           100,
			wantWorkingSet:           0,
			wantRSS:                  40,
		},
		{
			// Neither inactive_file nor total_inactive_file -> inactiveFile treated 0 -> WorkingSet == Usage.
			name: "NoInactiveFileKeyWorkingSetEqualsUsage",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{
					Usage: 250,
					Stats: map[string]uint64{"anon": 99},
				},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCores:            1_000_000_000,
			wantUsageBytes:           250,
			wantWorkingSet:           250,
			wantRSS:                  99,
		},
		{
			// Neither anon nor rss present -> RSS 0.
			name: "NoRSSKeyRSSZero",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{
					Usage: 200,
					Stats: map[string]uint64{"inactive_file": 60},
				},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCores:            1_000_000_000,
			wantUsageBytes:           200,
			wantWorkingSet:           140,
			wantRSS:                  0,
		},
		{
			// Both v2 and v1 keys present with distinct values -> v2 wins (anon over rss, inactive_file over total_inactive_file).
			// Pins precedence so a refactor flipping it is caught.
			name: "BothKeysPresentV2Wins",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 5_000_000_000},
					SystemUsage: 100_000_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 4_000_000_000},
					SystemUsage: 98_000_000_000,
				},
				types.MemoryStats{
					Usage: 1_000,
					Stats: map[string]uint64{
						"anon":                111,
						"rss":                 222,
						"inactive_file":       300,
						"total_inactive_file": 700,
					},
				},
			),
			wantUsageCoreNanoSeconds: 5_000_000_000,
			wantNanoCores:            1_000_000_000,
			wantUsageBytes:           1_000,
			wantWorkingSet:           700, // Usage - inactive_file (v2), not total_inactive_file
			wantRSS:                  111, // anon (v2), not rss
		},
		{
			// Real host numbers: cpuDelta=1_012_968_000, systemDelta=2_020_000_000, onlineCPUs=2.
			// float64(1012968000)/float64(2020000000)*2*1e9 = 1002938613.86 -> uint64 cast truncates.
			name: "RealHostNumbersExactCast",
			in: mkStatsJSON(
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 11_012_968_000},
					SystemUsage: 502_020_000_000,
					OnlineCPUs:  2,
				},
				types.CPUStats{
					CPUUsage:    types.CPUUsage{TotalUsage: 10_000_000_000},
					SystemUsage: 500_000_000_000,
				},
				types.MemoryStats{
					Usage: 123_456_789,
					Stats: map[string]uint64{"anon": 55_555_555, "inactive_file": 23_456_789},
				},
			),
			wantUsageCoreNanoSeconds: 11_012_968_000,
			wantNanoCores:            1_002_938_613,
			wantUsageBytes:           123_456_789,
			wantWorkingSet:           100_000_000, // 123_456_789 - 23_456_789
			wantRSS:                  55_555_555,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := dockerContainerStats("ctr-"+tt.name, tt.in, now)

			assert.Equal(t, "ctr-"+tt.name, got.Name)

			require.NotNil(t, got.CPU)
			assert.Equal(t, wantTime, got.CPU.Time)
			require.NotNil(t, got.CPU.UsageCoreNanoSeconds)
			assert.Equal(t, tt.wantUsageCoreNanoSeconds, *got.CPU.UsageCoreNanoSeconds)

			if tt.wantNanoCoresNil {
				assert.Nil(t, got.CPU.UsageNanoCores)
			} else {
				require.NotNil(t, got.CPU.UsageNanoCores)
				assert.Equal(t, tt.wantNanoCores, *got.CPU.UsageNanoCores)
			}

			require.NotNil(t, got.Memory)
			assert.Equal(t, wantTime, got.Memory.Time)
			require.NotNil(t, got.Memory.UsageBytes)
			assert.Equal(t, tt.wantUsageBytes, *got.Memory.UsageBytes)
			require.NotNil(t, got.Memory.WorkingSetBytes)
			assert.Equal(t, tt.wantWorkingSet, *got.Memory.WorkingSetBytes)
			require.NotNil(t, got.Memory.RSSBytes)
			assert.Equal(t, tt.wantRSS, *got.Memory.RSSBytes)
		})
	}
}
