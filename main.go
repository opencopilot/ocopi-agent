package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"

	docker "github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	pb "github.com/opencopilot/agent/agent"
	pbManager "github.com/opencopilot/agent/manager"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	"github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/tags"
)

var (
	// InstanceID is the identifier of this agent/device
	InstanceID = os.Getenv("INSTANCE_ID")
)

const (
	port = ":50051"
)

func servePublicGRPC() {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	logger, err := zap.NewProduction()
	defer logger.Sync()
	if err != nil {
		log.Fatalf("failed to setup logger: %v", err)
	}

	// TODO: TLS for gRPC connection to outside world
	// creds, err := credentials.NewServerTLSFromFile("server.crt", "server.key")
	// if err != nil {
	// 	log.Fatalf("failed to load credentials: %v", err)
	// }

	s := grpc.NewServer(
		// grpc.Creds(creds),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			grpc_zap.StreamServerInterceptor(logger),
			grpc_recovery.StreamServerInterceptor(),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			grpc_zap.UnaryServerInterceptor(logger),
			grpc_recovery.UnaryServerInterceptor(),
		)),
	)
	dockerCli, err := docker.NewEnvClient()
	if err != nil {
		log.Fatalf("failed to setup docker client on public gRPC server")
	}

	consulCli, err := consul.NewClient(consul.DefaultConfig())
	if err != nil {
		log.Fatalf("failed to setup consul client on public gRPC server")
	}

	pb.RegisterAgentServer(s, &server{
		dockerClient: *dockerCli,
		consulClient: *consulCli,
	})
	// Register reflection service on gRPC server.
	reflection.Register(s)
	s.Serve(lis)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

func servePrivateGRPC() {
	lis, err := net.Listen("tcp", "127.0.0.1:50050")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterAgentServer(s, &server{})

	// Register reflection service on gRPC server.
	reflection.Register(s)
	s.Serve(lis)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
func getManagerClient(port int) (pbManager.ManagerClient, *grpc.ClientConn) {
	conn, err := grpc.Dial(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithInsecure())
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	// defer conn.Close()
	return pbManager.NewManagerClient(conn), conn
}

func watchConfigTree(consulClient *consul.Client, prevIndex uint64, handler func(consul.KVPairs)) error {
	kv := consulClient.KV()
	kvs, queryMeta, err := kv.List("instances/"+InstanceID+"/services/", &consul.QueryOptions{
		WaitIndex: prevIndex,
	})
	if err != nil {
		return err
	}
	lastIndex := queryMeta.LastIndex
	if prevIndex != lastIndex {
		handler(kvs)
	}
	watchConfigTree(consulClient, lastIndex, handler)
	return nil
}

func main() {
	if InstanceID == "" {
		panic(errors.New("No instance ID specified"))
	}

	consulClient, err := consul.NewClient(consul.DefaultConfig())
	if err != nil {
		panic(err)
	}
	go watchConfigTree(consulClient, 0, ConfigHandler)

	log.Println("Starting public gRPC...")
	go servePublicGRPC()
	log.Println("Starting private gRPC...")
	servePrivateGRPC()
}