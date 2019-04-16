package consul

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/consul/agent/metadata"
	"github.com/hashicorp/consul/agent/pool"
	"github.com/hashicorp/consul/tlsutil"
	"google.golang.org/grpc"
)

const (
	grpcBasePath = "/consul"
)

type RPCClient struct {
	rpcPool   *pool.ConnPool
	grpcConns sync.Map
	logger    *log.Logger
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
		logger: logger,
	}
}

func (c *RPCClient) Call(dc string, server *metadata.Server, method string, args, reply interface{}) error {
	if server.GRPCPort <= 0 || !grpcAbleEndpoints[method] {
		c.logger.Printf("[TRACE] Using RPC for method %s", method)
		return c.rpcPool.RPC(dc, server.Addr, server.Version, method, server.UseTLS, args, reply)
	}

	conn, err := c.grpcConn(server)
	if err != nil {
		return err
	}

	c.logger.Printf("[TRACE] Using GRPC for method %s", method)
	return conn.Invoke(context.Background(), c.grpcPath(method), args, reply)
}

func (c *RPCClient) Ping(dc string, addr net.Addr, version int, useTLS bool) (bool, error) {
	return c.rpcPool.Ping(dc, addr, version, useTLS)
}

func (c *RPCClient) Shutdown() error {
	// Close the connection pool
	c.rpcPool.Shutdown()
	return nil
}

func (c *RPCClient) grpcConn(server *metadata.Server) (*grpc.ClientConn, error) {
	host, _, _ := net.SplitHostPort(server.Addr.String())
	addr := fmt.Sprintf("%s:%d", host, server.GRPCPort)

	conn, ok := c.grpcConns.Load(addr)
	if ok {
		return conn.(*grpc.ClientConn), nil
	}

	co, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	c.grpcConns.Store(addr, co)
	return co, nil
}

func (c *RPCClient) grpcPath(p string) string {
	return grpcBasePath + "." + strings.Replace(p, ".", "/", -1)
}
