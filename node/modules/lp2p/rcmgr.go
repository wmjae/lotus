package lp2p

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"

	ocprom "contrib.go.opencensus.io/exporter/prometheus"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	rcmgr "github.com/libp2p/go-libp2p-resource-manager"
	rcmgrObs "github.com/libp2p/go-libp2p-resource-manager/obs"
	"github.com/pbnjay/memory"
	"github.com/prometheus/client_golang/prometheus"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"go.uber.org/fx"

	"github.com/filecoin-project/lotus/lib/ulimit"
	"github.com/filecoin-project/lotus/metrics"
	"github.com/filecoin-project/lotus/node/repo"
)

func ResourceManager(connMgrHi uint) func(lc fx.Lifecycle, repo repo.LockedRepo) (network.ResourceManager, error) {
	return func(lc fx.Lifecycle, repo repo.LockedRepo) (network.ResourceManager, error) {
		isFullNode := repo.RepoType().Type() == "FullNode"
		envvar := os.Getenv("LOTUS_RCMGR")
		if (isFullNode && envvar == "0") || // only set NullResourceManager if envvar is explicitly "0"
			(!isFullNode && envvar != "1") { // set NullResourceManager *unless* envvar is explicitly "1"
			log.Info("libp2p resource manager is disabled")
			return network.NullResourceManager, nil
		}

		log.Info("libp2p resource manager is enabled")
		// enable debug logs for rcmgr
		logging.SetLogLevel("rcmgr", "debug")

		// Adjust default defaultLimits
		// - give it more memory, up to 4G, min of 1G
		// - if maxconns are too high, adjust Conn/FD/Stream defaultLimits
		defaultLimits := rcmgr.DefaultLimits

		// TODO: also set appropriate default limits for lotus protocols
		libp2p.SetDefaultServiceLimits(&defaultLimits)

		// Minimum 1GB of memory
		defaultLimits.SystemBaseLimit.Memory = 1 << 30
		// For every extra 1GB of memory we have available, increase our limit by 1GiB
		defaultLimits.SystemLimitIncrease.Memory = 1 << 30
		availFDs, _, err := ulimit.GetLimit()
		if err != nil {
			availFDs = 0
		}
		defaultLimitConfig := defaultLimits.Scale(int64(memory.TotalMemory())/8, int(availFDs>>1))
		if defaultLimitConfig.System.Memory > 4<<30 {
			// Cap our memory limit
			defaultLimitConfig.System.Memory = 4 << 30
		}

		maxconns := int(connMgrHi)
		if 2*maxconns > defaultLimitConfig.System.ConnsInbound {
			// adjust conns to 2x to allow for two conns per peer (TCP+QUIC)
			defaultLimitConfig.System.ConnsInbound = logScale(2 * maxconns)
			defaultLimitConfig.System.ConnsOutbound = logScale(2 * maxconns)
			defaultLimitConfig.System.Conns = logScale(4 * maxconns)

			defaultLimitConfig.System.StreamsInbound = logScale(16 * maxconns)
			defaultLimitConfig.System.StreamsOutbound = logScale(64 * maxconns)
			defaultLimitConfig.System.Streams = logScale(64 * maxconns)

			if 2*maxconns > defaultLimitConfig.System.FD {
				defaultLimitConfig.System.FD = logScale(2 * maxconns)
			}

			defaultLimitConfig.ServiceDefault.StreamsInbound = logScale(8 * maxconns)
			defaultLimitConfig.ServiceDefault.StreamsOutbound = logScale(32 * maxconns)
			defaultLimitConfig.ServiceDefault.Streams = logScale(32 * maxconns)

			defaultLimitConfig.ProtocolDefault.StreamsInbound = logScale(8 * maxconns)
			defaultLimitConfig.ProtocolDefault.StreamsOutbound = logScale(32 * maxconns)
			defaultLimitConfig.ProtocolDefault.Streams = logScale(32 * maxconns)

			log.Info("adjusted default resource manager limits")
		}

		// initialize
		var limiter rcmgr.Limiter
		var opts []rcmgr.Option

		repoPath := repo.Path()

		// create limiter -- parse $repo/limits.json if exists
		limitsFile := filepath.Join(repoPath, "limits.json")
		limitsIn, err := os.Open(limitsFile)
		switch {
		case err == nil:
			defer limitsIn.Close() //nolint:errcheck
			limiter, err = rcmgr.NewLimiterFromJSON(limitsIn, defaultLimitConfig)
			if err != nil {
				return nil, fmt.Errorf("error parsing limit file: %w", err)
			}

		case errors.Is(err, os.ErrNotExist):
			limiter = rcmgr.NewFixedLimiter(defaultLimitConfig)

		default:
			return nil, err
		}

		str, err := rcmgrObs.NewStatsTraceReporter()
		if err != nil {
			return nil, fmt.Errorf("error creating resource manager stats reporter: %w", err)
		}

		// Metrics
		opts = append(opts, rcmgr.WithMetrics(rcmgrMetrics{}), rcmgr.WithTraceReporter(str))

		if os.Getenv("LOTUS_DEBUG_RCMGR") != "" {
			debugPath := filepath.Join(repoPath, "debug")
			if err := os.MkdirAll(debugPath, 0755); err != nil {
				return nil, fmt.Errorf("error creating debug directory: %w", err)
			}
			traceFile := filepath.Join(debugPath, "rcmgr.json.gz")
			opts = append(opts, rcmgr.WithTrace(traceFile))
		}

		mgr, err := rcmgr.NewResourceManager(limiter, opts...)
		if err != nil {
			return nil, fmt.Errorf("error creating resource manager: %w", err)
		}

		// Hook up resource manager metrics
		err = view.Register(rcmgrObs.DefaultViews...)
		if err != nil {
			return nil, fmt.Errorf("error registering metrics: %w", err)
		}
		_, err = ocprom.NewExporter(ocprom.Options{
			Registry:  prometheus.DefaultRegisterer.(*prometheus.Registry),
			Namespace: "rcmgr_trace_metrics",
		})
		if err != nil {
			return nil, fmt.Errorf("error registering metrics: %w", err)
		}

		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				return mgr.Close()
			}})

		return mgr, nil
	}
}

func logScale(val int) int {
	bitlen := bits.Len(uint(val))
	return 1 << bitlen
}

func ResourceManagerOption(mgr network.ResourceManager) Libp2pOpts {
	return Libp2pOpts{
		Opts: []libp2p.Option{libp2p.ResourceManager(mgr)},
	}
}

type rcmgrMetrics struct{}

func (r rcmgrMetrics) AllowConn(dir network.Direction, usefd bool) {
	ctx := context.Background()
	if dir == network.DirInbound {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "inbound"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "outbound"))
	}
	if usefd {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.UseFD, "true"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.UseFD, "false"))
	}
	stats.Record(ctx, metrics.RcmgrAllowConn.M(1))
}

func (r rcmgrMetrics) BlockConn(dir network.Direction, usefd bool) {
	ctx := context.Background()
	if dir == network.DirInbound {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "inbound"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "outbound"))
	}
	if usefd {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.UseFD, "true"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.UseFD, "false"))
	}
	stats.Record(ctx, metrics.RcmgrBlockConn.M(1))
}

func (r rcmgrMetrics) AllowStream(p peer.ID, dir network.Direction) {
	ctx := context.Background()
	if dir == network.DirInbound {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "inbound"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "outbound"))
	}
	stats.Record(ctx, metrics.RcmgrAllowStream.M(1))
}

func (r rcmgrMetrics) BlockStream(p peer.ID, dir network.Direction) {
	ctx := context.Background()
	if dir == network.DirInbound {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "inbound"))
	} else {
		ctx, _ = tag.New(ctx, tag.Upsert(metrics.Direction, "outbound"))
	}
	stats.Record(ctx, metrics.RcmgrBlockStream.M(1))
}

func (r rcmgrMetrics) AllowPeer(p peer.ID) {
	ctx := context.Background()
	stats.Record(ctx, metrics.RcmgrAllowPeer.M(1))
}

func (r rcmgrMetrics) BlockPeer(p peer.ID) {
	ctx := context.Background()
	stats.Record(ctx, metrics.RcmgrBlockPeer.M(1))
}

func (r rcmgrMetrics) AllowProtocol(proto protocol.ID) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ProtocolID, string(proto)))
	stats.Record(ctx, metrics.RcmgrAllowProto.M(1))
}

func (r rcmgrMetrics) BlockProtocol(proto protocol.ID) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ProtocolID, string(proto)))
	stats.Record(ctx, metrics.RcmgrBlockProto.M(1))
}

func (r rcmgrMetrics) BlockProtocolPeer(proto protocol.ID, p peer.ID) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ProtocolID, string(proto)))
	stats.Record(ctx, metrics.RcmgrBlockProtoPeer.M(1))
}

func (r rcmgrMetrics) AllowService(svc string) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ServiceID, svc))
	stats.Record(ctx, metrics.RcmgrAllowSvc.M(1))
}

func (r rcmgrMetrics) BlockService(svc string) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ServiceID, svc))
	stats.Record(ctx, metrics.RcmgrBlockSvc.M(1))
}

func (r rcmgrMetrics) BlockServicePeer(svc string, p peer.ID) {
	ctx := context.Background()
	ctx, _ = tag.New(ctx, tag.Upsert(metrics.ServiceID, svc))
	stats.Record(ctx, metrics.RcmgrBlockSvcPeer.M(1))
}

func (r rcmgrMetrics) AllowMemory(size int) {
	stats.Record(context.Background(), metrics.RcmgrAllowMem.M(1))
}

func (r rcmgrMetrics) BlockMemory(size int) {
	stats.Record(context.Background(), metrics.RcmgrBlockMem.M(1))
}
