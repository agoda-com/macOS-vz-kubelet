package resourcemanager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	vmdata "github.com/agoda-com/macOS-vz-kubelet/internal/data/vm"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"

	"k8s.io/apimachinery/pkg/types"
)

type permitGuard struct {
	client    *MacOSClient
	key       types.NamespacedName
	committed atomic.Bool
	release   sync.Once
}

func (g *permitGuard) Commit() {
	g.committed.Store(true)
}

func (g *permitGuard) Release(ctx context.Context) {
	g.release.Do(func() {
		if g.committed.Load() {
			return
		}
		if g.client != nil {
			g.client.releasePermit(ctx, g.key)
		}
	})
}

// acquirePermit blocks until a capacity permit is available or the context is cancelled.
func (c *MacOSClient) acquirePermit(ctx context.Context, key types.NamespacedName) (*permitGuard, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case c.vmPermits <- struct{}{}:
			err := c.markPermitAcquired(key)
			if err != nil {
				<-c.vmPermits
				log.G(ctx).WithError(err).Warn("failed to mark VM permit as acquired")
				return nil, err
			}
			return &permitGuard{client: c, key: key}, nil
		case <-ticker.C:
			log.G(ctx).Debug("waiting for resources to be available")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *MacOSClient) markPermitAcquired(key types.NamespacedName) error {
	// permitClaimed is set inside the Compute callback, which executes synchronously,
	// so it is safe to read after Compute returns.
	permitClaimed := false
	_, found := c.vms.Update(key.Namespace, key.Name, func(info vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		if !info.PermitAcquired {
			info.PermitAcquired = true
			permitClaimed = true
		}
		return info
	})

	if !found {
		return errdefs.NotFound("virtual machine info expired")
	}
	if !permitClaimed {
		return errdefs.AsInvalidInput(fmt.Errorf("virtual machine permit already acquired"))
	}
	return nil
}

func (c *MacOSClient) releasePermit(ctx context.Context, key types.NamespacedName) {
	// shouldRelease is set inside the Compute callback, which executes synchronously,
	// so it is safe to read after Compute returns.
	shouldRelease := false
	_, found := c.vms.Update(key.Namespace, key.Name, func(info vmdata.VirtualMachineInfo) vmdata.VirtualMachineInfo {
		if info.PermitAcquired {
			info.PermitAcquired = false
			shouldRelease = true
		}
		return info
	})

	// Drain channel OUTSIDE Compute to avoid blocking while holding lock
	if shouldRelease {
		_, ok := <-c.vmPermits
		if !ok {
			log.G(ctx).WithField("namespace", key.Namespace).WithField("name", key.Name).
				Warn("expected to release VM permit but channel was closed")
		}
		return
	}

	if !found {
		select {
		case <-c.vmPermits:
			log.G(ctx).WithField("namespace", key.Namespace).WithField("name", key.Name).
				Debug("released VM permit without metadata present")
		default:
		}
	}
}
