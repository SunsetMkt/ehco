package node_metric

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

type Reader interface {
	ReadOnce(ctx context.Context) (*NodeMetrics, error)
}

type readerImpl struct {
	httpClient *http.Client
	metricsURL string
}

func NewReader(metricsURL string) *readerImpl {
	c := &http.Client{Timeout: 30 * time.Second}
	return &readerImpl{
		httpClient: c,
		metricsURL: metricsURL,
	}
}

func (b *readerImpl) parseCpuInfo(metricMap map[string]*dto.MetricFamily, nm *NodeMetrics) error {
	handleMetric := func(metricName string, handleValue func(float64, string)) error {
		metric, ok := metricMap[metricName]
		if !ok {
			return fmt.Errorf("%s not found", metricName)
		}

		for _, m := range metric.Metric {
			g := m.GetCounter()
			mode := ""
			for _, label := range m.GetLabel() {
				if label.GetName() == "mode" {
					mode = label.GetValue()
				}
			}
			handleValue(g.GetValue(), mode)
		}
		return nil
	}

	var (
		totalIdleTime float64
		totalCpuTime  float64
		cpuCores      int
	)

	err := handleMetric("node_cpu_seconds_total", func(val float64, mode string) {
		totalCpuTime += val
		if mode == "idle" {
			totalIdleTime += val
			cpuCores++
		}
	})
	if err != nil {
		return err
	}

	nm.CpuCoreCount = cpuCores
	nm.CpuUsagePercent = 100 * (totalCpuTime - totalIdleTime) / totalCpuTime
	for _, load := range []string{"1", "5", "15"} {
		loadMetricName := fmt.Sprintf("node_load%s", load)
		loadMetric, ok := metricMap[loadMetricName]
		if !ok {
			return fmt.Errorf("%s not found", loadMetricName)
		}
		for _, m := range loadMetric.Metric {
			g := m.GetGauge()
			nm.CpuLoadInfo += fmt.Sprintf("%.2f|", g.GetValue())
		}
	}
	nm.CpuLoadInfo = strings.TrimRight(nm.CpuLoadInfo, "|")
	return nil
}

func (b *readerImpl) parseMemoryInfo(metricMap map[string]*dto.MetricFamily, nm *NodeMetrics) error {
	handleMetric := func(metricName string, handleValue func(float64)) error {
		metric, ok := metricMap[metricName]
		if !ok {
			return fmt.Errorf("%s not found", metricName)
		}
		for _, m := range metric.Metric {
			g := m.GetGauge()
			handleValue(g.GetValue())
		}
		return nil
	}

	isMac := false
	if _, ok := metricMap["node_memory_total_bytes"]; ok {
		isMac = true
	}

	if isMac {
		err := handleMetric("node_memory_total_bytes", func(val float64) {
			nm.MemoryTotalBytes = val
		})
		if err != nil {
			return err
		}

		err = handleMetric("node_memory_active_bytes", func(val float64) {
			nm.MemoryUsageBytes += val
		})
		if err != nil {
			return err
		}

		err = handleMetric("node_memory_wired_bytes", func(val float64) {
			nm.MemoryUsageBytes += val
		})
		if err != nil {
			return err
		}
	} else {
		err := handleMetric("node_memory_MemTotal_bytes", func(val float64) {
			nm.MemoryTotalBytes = val
		})
		if err != nil {
			return err
		}

		err = handleMetric("node_memory_MemAvailable_bytes", func(val float64) {
			nm.MemoryUsageBytes = nm.MemoryTotalBytes - val
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func getDiskName(devicePath string) string {
	// parse disk name from device path,such as:
	// e.g. /dev/disk1 -> disk1
	// e.g. ntfs://disk1 -> disk1
	// e.g. /dev/sda -> sda
	re := regexp.MustCompile(`/dev/disk(\d+)|ntfs://disk(\d+)|/dev/sd[a-zA-Z]`)
	return re.FindString(devicePath)
}

func (b *readerImpl) parseDiskInfo(metricMap map[string]*dto.MetricFamily, nm *NodeMetrics) error {
	handleMetric := func(metricName string, handleValue func(float64)) error {
		forMac := false
		diskMap := make(map[string]float64)
		metric, ok := metricMap[metricName]
		if !ok {
			return fmt.Errorf("%s not found", metricName)
		}
		for _, m := range metric.Metric {
			g := m.GetGauge()
			disk := ""
			for _, label := range m.GetLabel() {
				if label.GetName() == "device" {
					disk = getDiskName(label.GetValue())
				}
				if label.GetName() == "fstype" && label.GetValue() == "apfs" {
					forMac = true
				}
			}
			diskMap[disk] = g.GetValue()
		}
		// 对于 macos 的 apfs 文件系统，可能会有多个相同大小的磁盘，这是因为 apfs 磁盘（卷）会共享物理磁盘
		seenVal := map[float64]bool{}
		for _, val := range diskMap {
			if seenVal[val] && forMac {
				continue
			}
			handleValue(val)
			seenVal[val] = true
		}
		return nil
	}

	err := handleMetric("node_filesystem_size_bytes", func(val float64) {
		nm.DiskTotalBytes += val
	})
	if err != nil {
		return err
	}

	var availBytes float64
	err = handleMetric("node_filesystem_avail_bytes", func(val float64) {
		availBytes += val
	})
	if err != nil {
		return err
	}
	nm.DiskUsageBytes = nm.DiskTotalBytes - availBytes
	return nil
}

func (b *readerImpl) parseNetworkInfo(metricMap map[string]*dto.MetricFamily, nm *NodeMetrics) error {
	handleMetric := func(metricName string, handleValue func(float64)) error {
		metric, ok := metricMap[metricName]
		if !ok {
			return fmt.Errorf("%s not found", metricName)
		}
		for _, m := range metric.Metric {
			g := m.GetCounter()
			handleValue(g.GetValue())
		}
		return nil
	}

	err := handleMetric("node_network_receive_bytes_total", func(val float64) {
		nm.NetworkReceiveBytesTotal += val
	})
	if err != nil {
		return err
	}

	err = handleMetric("node_network_transmit_bytes_total", func(val float64) {
		nm.NetworkTransmitBytesTotal += val
	})
	if err != nil {
		return err
	}
	return nil
}

func (b *readerImpl) ReadOnce(ctx context.Context) (*NodeMetrics, error) {
	response, err := b.httpClient.Get(b.metricsURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var parser expfmt.TextParser
	parsed, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	nm := &NodeMetrics{}
	if err := b.parseCpuInfo(parsed, nm); err != nil {
		return nil, err
	}
	if err := b.parseMemoryInfo(parsed, nm); err != nil {
		return nil, err
	}
	if err := b.parseDiskInfo(parsed, nm); err != nil {
		return nil, err
	}
	if err := b.parseNetworkInfo(parsed, nm); err != nil {
		return nil, err
	}
	return nm, nil
}
