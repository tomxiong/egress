package stats

import (
	"runtime"
	"sort"
	"time"

	"github.com/frostbyte73/go-throttle"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

type Monitor struct {
	cpuCostConfig config.CPUCostConfig

	promCPULoad  prometheus.Gauge
	requestGauge *prometheus.GaugeVec

	cpuStats *utils.CPUStats

	pendingCPUs     atomic.Float64
	numCPUs         float64
	warningThrottle func(func())
}

func NewMonitor() *Monitor {
	return &Monitor{
		numCPUs:         float64(runtime.NumCPU()),
		warningThrottle: throttle.New(time.Minute),
	}
}

func (m *Monitor) Start(conf *config.Config, isAvailable func() float64) error {
	if err := m.checkCPUConfig(conf.CPUCost); err != nil {
		return err
	}

	promNodeAvailable := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "egress",
		Name:        "available",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, isAvailable)

	m.promCPULoad = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "node",
		Name:        "cpu_load",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID, "node_type": "EGRESS"},
	})

	m.requestGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   "livekit",
		Subsystem:   "egress",
		Name:        "requests",
		ConstLabels: prometheus.Labels{"node_id": conf.NodeID},
	}, []string{"type"})

	prometheus.MustRegister(promNodeAvailable, m.promCPULoad, m.requestGauge)

	cpuStats, err := utils.NewCPUStats(func(idle float64) {
		m.promCPULoad.Set(1 - idle/m.numCPUs)
	})
	if err != nil {
		return err
	}

	m.cpuStats = cpuStats

	return nil
}

func (m *Monitor) checkCPUConfig(costConfig config.CPUCostConfig) error {
	if costConfig.RoomCompositeCpuCost < 2.5 {
		logger.Warnw("room composite requirement too low", nil,
			"config value", costConfig.RoomCompositeCpuCost,
			"minimum value", 2.5,
			"recommended value", 3,
		)
	}
	if costConfig.WebCpuCost < 2.5 {
		logger.Warnw("web requirement too low", nil,
			"config value", costConfig.WebCpuCost,
			"minimum value", 2.5,
			"recommended value", 3,
		)
	}
	if costConfig.TrackCompositeCpuCost < 1 {
		logger.Warnw("track composite requirement too low", nil,
			"config value", costConfig.TrackCompositeCpuCost,
			"minimum value", 1,
			"recommended value", 2,
		)
	}
	if costConfig.TrackCpuCost < 0.5 {
		logger.Warnw("track requirement too low", nil,
			"config value", costConfig.RoomCompositeCpuCost,
			"minimum value", 0.5,
			"recommended value", 1,
		)
	}

	requirements := []float64{
		costConfig.RoomCompositeCpuCost,
		costConfig.WebCpuCost,
		costConfig.TrackCompositeCpuCost,
		costConfig.TrackCpuCost,
	}
	sort.Float64s(requirements)

	recommendedMinimum := requirements[2]
	if recommendedMinimum < 3 {
		recommendedMinimum = 3
	}

	if m.numCPUs < requirements[0] {
		logger.Errorw("not enough cpu", nil,
			"minimum cpu", requirements[0],
			"recommended", recommendedMinimum,
			"available", m.numCPUs,
		)
		return errors.New("not enough cpu")
	}

	if m.numCPUs < requirements[3] {
		logger.Errorw("not enough cpu for some egress types", nil,
			"minimum cpu", requirements[3],
			"recommended", recommendedMinimum,
			"available", m.numCPUs,
		)
	}

	return nil
}

func (m *Monitor) GetCPULoad() float64 {
	return (m.numCPUs - m.cpuStats.GetCPUIdle()) / m.numCPUs * 100
}

func (m *Monitor) CanAcceptRequest(req *livekit.StartEgressRequest) bool {
	accept := false
	available := m.cpuStats.GetCPUIdle() - m.pendingCPUs.Load()

	switch req.Request.(type) {
	case *livekit.StartEgressRequest_RoomComposite:
		accept = available > m.cpuCostConfig.RoomCompositeCpuCost
	case *livekit.StartEgressRequest_Web:
		accept = available > m.cpuCostConfig.WebCpuCost
	case *livekit.StartEgressRequest_TrackComposite:
		accept = available > m.cpuCostConfig.TrackCompositeCpuCost
	case *livekit.StartEgressRequest_Track:
		accept = available > m.cpuCostConfig.TrackCpuCost
	}

	logger.Debugw("cpu request", "accepted", accept, "availableCPUs", available, "numCPUs", runtime.NumCPU())
	return accept
}

func (m *Monitor) AcceptRequest(req *livekit.StartEgressRequest) {
	var cpuHold float64
	switch req.Request.(type) {
	case *livekit.StartEgressRequest_RoomComposite:
		cpuHold = m.cpuCostConfig.RoomCompositeCpuCost
	case *livekit.StartEgressRequest_Web:
		cpuHold = m.cpuCostConfig.WebCpuCost
	case *livekit.StartEgressRequest_TrackComposite:
		cpuHold = m.cpuCostConfig.TrackCompositeCpuCost
	case *livekit.StartEgressRequest_Track:
		cpuHold = m.cpuCostConfig.TrackCpuCost
	}

	m.pendingCPUs.Add(cpuHold)
	time.AfterFunc(time.Second, func() { m.pendingCPUs.Sub(cpuHold) })
}

func (m *Monitor) EgressStarted(req *livekit.StartEgressRequest) {
	switch req.Request.(type) {
	case *livekit.StartEgressRequest_RoomComposite:
		m.requestGauge.With(prometheus.Labels{"type": "room_composite"}).Add(1)
	case *livekit.StartEgressRequest_Web:
		m.requestGauge.With(prometheus.Labels{"type": "web"}).Add(1)
	case *livekit.StartEgressRequest_TrackComposite:
		m.requestGauge.With(prometheus.Labels{"type": "track_composite"}).Add(1)
	case *livekit.StartEgressRequest_Track:
		m.requestGauge.With(prometheus.Labels{"type": "track"}).Add(1)
	}
}

func (m *Monitor) EgressEnded(req *livekit.StartEgressRequest) {
	switch req.Request.(type) {
	case *livekit.StartEgressRequest_RoomComposite:
		m.requestGauge.With(prometheus.Labels{"type": "room_composite"}).Sub(1)
	case *livekit.StartEgressRequest_Web:
		m.requestGauge.With(prometheus.Labels{"type": "web"}).Sub(1)
	case *livekit.StartEgressRequest_TrackComposite:
		m.requestGauge.With(prometheus.Labels{"type": "track_composite"}).Sub(1)
	case *livekit.StartEgressRequest_Track:
		m.requestGauge.With(prometheus.Labels{"type": "track"}).Sub(1)
	}
}
