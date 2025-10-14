package ssh_test

import (
	"context"
	"net"
	"testing"
	"time"

	vzssh "github.com/agoda-com/macOS-vz-kubelet/internal/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/testdata"
)

func TestDialContext(t *testing.T) {
	addr := "127.0.0.1:2222"
	listener := startMockSSHServer(t, addr)
	defer func() {
		assert.NoError(t, listener.Close())
	}()

	config := &ssh.ClientConfig{
		User:            "testuser",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	t.Run("successful connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", addr, config)
		assert.NoError(t, err)
		assert.NotNilf(t, client, "Expected client, got nil")
		require.NoError(t, client.Close())
	})

	t.Run("context timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", addr, config)
		assert.Errorf(t, err, "Expected error due to context timeout, got nil")
		assert.Nilf(t, client, "Expected nil client, got %v", client)
		assert.EqualErrorf(t, err, context.DeadlineExceeded.Error(), "Expected context.DeadlineExceeded, got %v", err)
	})

	t.Run("connection error", func(t *testing.T) {
		invalidAddr := "127.0.0.1:9999"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		client, err := vzssh.DialContext(ctx, "tcp", invalidAddr, config)
		assert.Errorf(t, err, "Expected error due to invalid address, got nil")
		assert.Nilf(t, client, "Expected nil client, got %v", client)
	})
}

// Mock SSH server to simulate ssh.ClientConn
func startMockSSHServer(t *testing.T, addr string) net.Listener {
	t.Helper()

	privateBytes := testdata.PEMBytes["rsa"]
	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		t.Fatalf("Failed to parse private key: %v", err)
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	config.AddHostKey(private)

	listenConfig := &net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go handleChannels(t, chans)
			}()
		}
	}()
	return listener
}

// Handle channels for mock SSH server
func handleChannels(t *testing.T, chans <-chan ssh.NewChannel) {
	t.Helper()

	for newChannel := range chans {
		channel, _, err := newChannel.Accept()
		require.NoError(t, err)
		require.NoError(t, channel.Close())
	}
}
