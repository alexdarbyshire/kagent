package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
)

func TestServerUsesExplicitInheritedListener(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer listener.Close()
	file, err := listener.File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	// The launcher hands over a raw descriptor with no second os.File owner.
	fd, err := syscall.Dup(int(file.Fd()))
	require.NoError(t, err)
	t.Setenv("KAGENT_A2A_LISTEN_FD", strconv.Itoa(fd))
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	server, err := NewA2AServer(a2atype.AgentCard{Name: "inherited"}, substrateExecutor{}, slog.New(slog.DiscardHandler), ServerConfig{Host: "127.0.0.1", Port: port})
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		_ = server.httpServer.Shutdown(context.Background())
		_ = server.readyServer.Shutdown(context.Background())
		server.grpcServer.Stop()
	})
	response, err := http.Get("http://" + listener.Addr().String() + "/health")
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}

func TestServerRefusesInvalidInheritedDescriptor(t *testing.T) {
	for _, fd := range []string{"", "garbage", "-1", "0", "999999"} {
		t.Run(fd, func(t *testing.T) {
			t.Setenv("KAGENT_A2A_LISTEN_FD", fd)
			server, err := NewA2AServer(a2atype.AgentCard{Name: "invalid"}, substrateExecutor{}, slog.New(slog.DiscardHandler), ServerConfig{Port: "0"})
			require.NoError(t, err)
			require.ErrorContains(t, server.Start(), "inherited A2A listener")
		})
	}
	regular, err := os.CreateTemp(t.TempDir(), "not-a-socket")
	require.NoError(t, err)
	defer regular.Close()
	t.Setenv("KAGENT_A2A_LISTEN_FD", strconv.Itoa(int(regular.Fd())))
	server, err := NewA2AServer(a2atype.AgentCard{Name: "invalid"}, substrateExecutor{}, slog.New(slog.DiscardHandler), ServerConfig{Port: "0"})
	require.NoError(t, err)
	require.ErrorContains(t, server.Start(), "inherited A2A listener")
}
