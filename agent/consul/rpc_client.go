package consul

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/agent/pool"
	"github.com/hashicorp/consul/tlsutil"
	"google.golang.org/grpc"
)

const (
	grpcBasePath = "/consul"
)

func dialGRPC(addr string, _ time.Duration) (net.Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	_, err = conn.Write([]byte{pool.RPCGRPC})
	if err != nil {
		return nil, err
	}

	return conn, nil
}

type connSlot struct {
	conn *grpc.ClientConn
	err  error
	done chan struct{}
}

type RPCClient struct {
	rpcPool       *pool.ConnPool
	grpcConns     map[string]*connSlot
	grpcConnsLock sync.RWMutex
	logger        *log.Logger
}

func NewRPCClient(logger *log.Logger, config *Config, tlsConfigurator *tlsutil.Configurator, maxConns int, maxIdleTime time.Duration) *RPCClient {
	return &RPCClient{
		rpcPool: &pool.ConnPool{
			SrcAddr:    config.RPCSrcAddr,
			LogOutput:  config.LogOutput,
			MaxTime:    maxIdleTime,
			MaxStreams: maxConns,
			TLSWrapper: tlsConfigurator.OutgoingRPCWrapper(),
			ForceTLS:   config.VerifyOutgoing,
		},
		grpcConns: make(map[string]*connSlot),
		logger:    logger,
	}
}

// Call swith between GRPC/RPC calls
func (c *RPCClient) Call(dc string, server *metadata.Server, method string, args, reply interface{}) error {
	isRPC := !server.GRPCEnabled || !grpcAbleEndpoints[method]
	var methodKind = "grpc"
	if isRPC {
		methodKind = "rpc"
	}

	var err error

	defer func() {
		metrics.IncrCounterWithLabels([]string{"client", "rpc", "dispatcher", "method"}, 1,
			[]metrics.Label{{Name: "method", Value: method},
				{Name: "kind", Value: methodKind},
				{Name: "dc", Value: dc},
				{Name: "destination", Value: server.Addr.String()},
				{Name: "error", Value: fmt.Sprintf("%v", err != nil)},
			})
	}()

	if isRPC {
		err = c.rpcPool.RPC(dc, server.Addr, server.Version, method, server.UseTLS, args, reply)
		return err
	}

	var conn *grpc.ClientConn

	conn, err = c.grpcConn(server)
	if err != nil {
		return err
	}

	err = conn.Invoke(context.Background(), c.grpcPath(method), args, reply)
	return err
}

// Ping ensure the remote server is alive
func (c *RPCClient) Ping(dc string, addr net.Addr, version int, useTLS bool) (bool, error) {
	return c.rpcPool.Ping(dc, addr, version, useTLS)
}

// Shutdown Close the connection pool
func (c *RPCClient) Shutdown() error {
	c.rpcPool.Shutdown()
	return nil
}

func (c *RPCClient) grpcConn(server *metadata.Server) (*grpc.ClientConn, error) {
	host, _, _ := net.SplitHostPort(server.Addr.String())
	addr := fmt.Sprintf("%s:%d", host, server.Port)

	var isFirstRequest bool

	c.grpcConnsLock.Lock()
	existing, ok := c.grpcConns[addr]
	if !ok {
		existing = &connSlot{
			done: make(chan struct{}),
		}
		c.grpcConns[addr] = existing
		isFirstRequest = true
	}
	c.grpcConnsLock.Unlock()

	if !isFirstRequest {
		<-existing.done
		return existing.conn, existing.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithInsecure(),
		grpc.WithDialer(dialGRPC),
		grpc.WithDisableRetry(),
		grpc.WithBlock(),
	)
	existing.conn = conn
	existing.err = err
	close(existing.done)

	return conn, err

}

func (c *RPCClient) grpcPath(p string) string {
	return grpcBasePath + "." + strings.Replace(p, ".", "/", -1)
}
