package grpc

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/api/metadata"
	ic "github.com/go-kratos/kratos/v2/internal/context"
	"github.com/go-kratos/kratos/v2/internal/host"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/recovery"
	"github.com/go-kratos/kratos/v2/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var _ transport.Server = (*Server)(nil)
var _ transport.Endpointer = (*Server)(nil)

// ServerOption is gRPC server option.
type ServerOption func(o *Server)

// Network with server network.
func Network(network string) ServerOption {
	return func(s *Server) {
		s.network = network
	}
}

// Address with server address.
func Address(addr string) ServerOption {
	return func(s *Server) {
		s.address = addr
	}
}

// Timeout with server timeout.
func Timeout(timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.timeout = timeout
	}
}

// Logger with server logger.
func Logger(logger log.Logger) ServerOption {
	return func(s *Server) {
		s.log = log.NewHelper(logger)
	}
}

// Middleware with server middleware.
func Middleware(m ...middleware.Middleware) ServerOption {
	return func(s *Server) {
		s.middleware = middleware.Chain(m...)
	}
}

// Options with grpc options.
func Options(opts ...grpc.ServerOption) ServerOption {
	return func(s *Server) {
		s.grpcOpts = opts
	}
}

// Server is a gRPC server wrapper.
type Server struct {
	*grpc.Server
	ctx        context.Context
	lis        net.Listener
	network    string
	address    string
	endpoint   *url.URL
	timeout    time.Duration
	log        *log.Helper
	middleware middleware.Middleware
	grpcOpts   []grpc.ServerOption
	health     *health.Server
	metadata   *metadata.Server
}

// NewServer creates a gRPC server by options.
func NewServer(opts ...ServerOption) *Server {
	srv := &Server{
		network: "tcp",
		address: ":0",
		timeout: 1 * time.Second,
		middleware: middleware.Chain(
			recovery.Recovery(),
		),
		health: health.NewServer(),
		log:    log.NewHelper(log.DefaultLogger),
	}
	for _, o := range opts {
		o(srv)
	}
	var grpcOpts = []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			srv.unaryServerInterceptor(),
		),
	}
	if len(srv.grpcOpts) > 0 {
		grpcOpts = append(grpcOpts, srv.grpcOpts...)
	}
	srv.Server = grpc.NewServer(grpcOpts...)
	srv.metadata = metadata.NewServer(srv.Server)
	// internal register
	grpc_health_v1.RegisterHealthServer(srv.Server, srv.health)
	metadata.RegisterMetadataServer(srv.Server, srv.metadata)
	reflection.Register(srv.Server)
	return srv
}

// Endpoint return a real address to registry endpoint.
// examples:
//   grpc://127.0.0.1:9000?isSecure=false
func (s *Server) Endpoint() (*url.URL, error) {
	if s.lis == nil && strings.HasSuffix(s.address, ":0") {
		lis, err := net.Listen(s.network, s.address)
		if err != nil {
			return nil, err
		}
		s.lis = lis
	}
	addr, err := host.Extract(s.address, s.lis)
	if err != nil {
		return nil, err
	}
	u := &url.URL{
		Scheme: "grpc",
		Host:   addr,
	}
	s.endpoint = u
	return u, nil
}

// Start start the gRPC server.
func (s *Server) Start(ctx context.Context) error {
	s.ctx = ctx
	if s.lis == nil {
		lis, err := net.Listen(s.network, s.address)
		if err != nil {
			return err
		}
		s.lis = lis
	}
	s.log.Infof("[gRPC] server listening on: %s", s.lis.Addr().String())
	s.health.Resume()
	return s.Serve(s.lis)
}

// Stop stop the gRPC server.
func (s *Server) Stop(ctx context.Context) error {
	s.GracefulStop()
	s.health.Shutdown()
	s.log.Info("[gRPC] server stopping")
	return nil
}

func (s *Server) unaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		ctx, cancel := ic.Merge(ctx, s.ctx)
		defer cancel()
		ctx = transport.NewContext(ctx, transport.Transport{Kind: transport.KindGRPC})
		ctx = NewServerContext(ctx, ServerInfo{Server: info.Server, FullMethod: info.FullMethod, Endpoint: s.endpoint})
		if s.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.timeout)
			defer cancel()
		}
		h := func(ctx context.Context, req interface{}) (interface{}, error) {
			return handler(ctx, req)
		}
		if s.middleware != nil {
			h = s.middleware(h)
		}
		return h(ctx, req)
	}
}
