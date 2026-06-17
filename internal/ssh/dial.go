// Note: Remove when proposal https://go-review.googlesource.com/c/crypto/+/550096 is merged
package ssh

import (
	"context"
	"net"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/trace"

	"golang.org/x/crypto/ssh"
)

// DialContext starts a client connection to the given SSH server. It is a
// convenience function that connects to the given network address,
// initiates the SSH handshake, and then sets up a Client.
//
// The provided Context must be non-nil. If the context expires before the
// connection is complete, an error is returned. Once successfully connected,
// any expiration of the context will not affect the connection.
//
// When config.Timeout > 0 it bounds connect AND handshake as one budget,
// independent of the (longer) caller ctx (see envcfg.SSHDialTimeout).
//
// See [Dial] for additional information.
func DialContext(ctx context.Context, network, addr string, config *ssh.ClientConfig) (client *ssh.Client, err error) {
	ctx, span := trace.StartSpan(ctx, "VZSSH.Dial")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	if config.Timeout > 0 {
		// Bound connect+handshake as one budget independent of the (longer) caller
		// ctx: a stalled guest login hangs NewClientConn past the TCP connect, and
		// only the caller ctx guarded the handshake before. The handshake select on
		// ctx.Done() below then observes this deadline.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
		defer cancel()
	}

	d := net.Dialer{
		Timeout: config.Timeout,
	}
	tcpCtx, tcpSpan := trace.StartSpan(ctx, "VZSSH.DialTCP")
	conn, err := d.DialContext(tcpCtx, network, addr)
	tcpSpan.SetStatus(err)
	tcpSpan.End()
	if err != nil {
		return nil, err
	}
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result)
	_, hsSpan := trace.StartSpan(ctx, "VZSSH.Handshake")
	go func() {
		var client *ssh.Client
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
		if err == nil {
			client = ssh.NewClient(c, chans, reqs)
		}
		select {
		case ch <- result{client, err}:
		case <-ctx.Done():
			if client != nil {
				if err := client.Close(); err != nil {
					log.G(ctx).WithError(err).Error("Failed to close client")
				}
			}
		}
	}()
	select {
	case res := <-ch:
		hsSpan.SetStatus(res.err)
		hsSpan.End()
		return res.client, res.err
	case <-ctx.Done():
		err = context.Cause(ctx)
		// Close the raw conn so a timed-out handshake can't leak it (and the
		// in-flight NewClientConn goroutine unwinds).
		_ = conn.Close()
		hsSpan.SetStatus(err)
		hsSpan.End()
		return nil, err
	}
}
