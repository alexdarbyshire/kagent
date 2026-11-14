package mcp

import (
	"context"
	"sync"
)

type runtimeClientsKey struct{}

type runtimeClient struct {
	stop func()
	join func(context.Context)
}

type runtimeClients struct {
	mu      sync.Mutex
	closed  bool
	clients []runtimeClient
	once    sync.Once
}

// WithClientLifecycleShutdown joins MCP teardown before the runtime returns.
func WithClientLifecycleShutdown(ctx context.Context) (context.Context, func()) {
	clients := &runtimeClients{}
	return context.WithValue(ctx, runtimeClientsKey{}, clients), clients.close
}

func (r *runtimeClients) register(client runtimeClient) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.clients = append(r.clients, client)
	return true
}

func (r *runtimeClients) close() {
	r.once.Do(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		r.mu.Lock()
		r.closed = true
		clients := r.clients
		r.clients = nil
		for _, client := range clients {
			client.stop()
		}
		r.mu.Unlock()
		var joined sync.WaitGroup
		for _, client := range clients {
			joined.Go(func() { client.join(shutdown) })
		}
		joined.Wait()
	})
}
