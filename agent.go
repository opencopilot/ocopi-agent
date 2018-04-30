package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"strconv"

	"github.com/docker/docker/api/types/filters"
	"google.golang.org/grpc"

	"github.com/buger/jsonparser"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerClient "github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	pb "github.com/opencopilot/agent/agent"
	managerPb "github.com/opencopilot/agent/manager"
	consulkvjson "github.com/opencopilot/consul-kv-json"
)

// Service is a specification for a service running on the device
type Service string

// Services is a list of Service
type Services []Service

// AgentGetStatus returns the status of a running service
func AgentGetStatus(ctx context.Context) (*pb.AgentStatus, error) {
	dockerCli, err := dockerClient.NewEnvClient()
	if err != nil {
		return nil, err
	}
	status := &pb.AgentStatus{InstanceId: InstanceID, Services: []*pb.AgentStatus_AgentService{}}

	containers, err := dockerCli.ContainerList(ctx, dockerTypes.ContainerListOptions{})
	if err != nil {
		log.Fatal(err)
	}

	for _, container := range containers {
		status.Services = append(status.Services, &pb.AgentStatus_AgentService{Id: container.ID, Image: container.Image})
	}

	return status, nil
}

// ConfigHandler runs when the service configuration for this instance changes
func ConfigHandler(kvs consul.KVPairs) {
	HandlingServices = true
	m, err := consulkvjson.ConsulKVsToJSON(kvs)
	if err != nil {
		log.Panic(err)
	}
	jsonString, err := json.Marshal(m)
	if err != nil {
		log.Panic(err)
	}
	// fmt.Printf("%s, %v", jsonString, m)
	_, valueType, _, err := jsonparser.Get(jsonString, "instances", InstanceID, "services")
	if valueType == jsonparser.NotExist {
		ensureServices(Services{})
		return
	}

	if err != nil {
		log.Fatal(err)
	}

	type mt = map[string]interface{}
	servicesMap := m["instances"].(mt)[InstanceID].(mt)["services"].(mt)
	incomingServices := Services{}
	for s := range servicesMap {
		service := Service(s)
		incomingServices = append(incomingServices, service)
	}

	ensureServices(incomingServices)
	localServices, err := getLocalServices()
	if err != nil {
		log.Fatal(err)
	}
	configureServices(localServices)

	HandlingServices = false
}

func getLocalServices() (Services, error) {
	cli, err := dockerClient.NewEnvClient()
	if err != nil {
		return nil, err
	}

	args := filters.NewArgs(
		filters.Arg("label", "com.opencopilot.managed"),
	)
	containers, err := cli.ContainerList(context.Background(), dockerTypes.ContainerListOptions{
		Filters: args,
	})
	if err != nil {
		log.Panic(err)
	}

	localServices := Services{}
	for _, container := range containers {
		serviceName, found := container.Labels["com.opencopilot.service-manager"]
		if !found {
			continue
		}
		// fmt.Printf("%s %s %v\n", container.ID[:10], container.Image, serviceName)
		service := Service(serviceName)
		localServices = append(localServices, service)
	}
	return localServices, nil
}

func ensureServices(incomingServices Services) {
	localServices, err := getLocalServices()
	if err != nil {
		log.Panicln(err)
	}

	for _, incomingService := range incomingServices {
		// For every service we should have, go check all the services we're currently running
		existsLocally := false
		for _, localService := range localServices {
			// If an incoming service is already running, do nothing
			if incomingService == localService {
				existsLocally = true
			}
		}

		// If we didn't find that this incoming service exists locally, start it
		if existsLocally {
			break
		} else {
			err := startService(incomingService)
			if err != nil {
				// TODO: do something else here
				log.Println(err)
			}
		}
	}

	for _, localService := range localServices {
		// For every locally running service, check if it should be running from the incoming services
		existsIncoming := false
		for _, incomingService := range incomingServices {
			if localService == incomingService {
				existsIncoming = true
			}
		}

		// If we didn't find that this local service exists in the incoming specification, stop it
		if existsIncoming {
			break
		} else {
			err := stopService(localService)
			if err != nil {
				// TODO: do something else here
				log.Println(err)
			}
		}

	}
}

func startService(service Service) error {
	log.Printf("adding service: %s\n", string(service))

	dockerCli, err := dockerClient.NewEnvClient()
	if err != nil {
		return err
	}

	ctx := context.Background()

	var containerConfig *container.Config
	switch string(service) {
	case "LB":
		containerConfig = &container.Config{
			Image: "quay.io/opencopilot/haproxy-manager",
			Labels: map[string]string{
				"com.opencopilot.managed":         "",
				"com.opencopilot.service-manager": string(service),
			},
		}
	default:
		return errors.New("Invalid service specified")
	}

	reader, err := dockerCli.ImagePull(ctx, containerConfig.Image, dockerTypes.ImagePullOptions{})
	if err != nil {
		return err
	}

	defer reader.Close()
	if _, err := ioutil.ReadAll(reader); err != nil {
		log.Panic(err)
	}

	containerConfig.Env = []string{"CONFIG_DIR=" + ConfigDir, "INSTANCE_ID=" + InstanceID}

	res, err := dockerCli.ContainerCreate(ctx, containerConfig, &container.HostConfig{
		AutoRemove: true, // Important to remove container after it's stopped, so that we can start a new one up with the same name if this service gets re-added
		Privileged: true, // So that the manager containers can start other docker containers,
		Binds: []string{ // So that the manager containers have access to Docker on the host
			"/var/run/docker.sock:/var/run/docker.sock",
			ConfigDir + ":" + ConfigDir,
		},
		PublishAllPorts: true,
	}, nil, "com.opencopilot.service-manager."+string(service))
	if err != nil {
		return err
	}

	startErr := dockerCli.ContainerStart(ctx, res.ID, dockerTypes.ContainerStartOptions{})
	if startErr != nil {
		return startErr
	}

	return nil
}

func stopService(service Service) error {
	log.Printf("stopping service: %s\n", string(service))
	dockerCli, err := dockerClient.NewEnvClient()
	if err != nil {
		return err
	}

	ctx := context.Background()
	args := filters.NewArgs(
		filters.Arg("label", "com.opencopilot.managed"),
		filters.Arg("name", "com.opencopilot.service-manager."+string(service)),
	)
	containers, err := dockerCli.ContainerList(ctx, dockerTypes.ContainerListOptions{
		Filters: args,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, container := range containers {
		dockerCli.ContainerStop(ctx, container.ID, nil)
	}

	return nil
}

func configureService(service Service) error {
	// log.Printf("configuring service: %s\n", string(service))
	dockerCli, err := dockerClient.NewEnvClient()
	if err != nil {
		return err
	}

	ctx := context.Background()
	args := filters.NewArgs(
		filters.Arg("label", "com.opencopilot.managed"),
		filters.Arg("name", "com.opencopilot.service-manager."+string(service)),
	)
	containers, err := dockerCli.ContainerList(ctx, dockerTypes.ContainerListOptions{
		Filters: args,
	})
	if err != nil {
		log.Fatal(err)
	}

	consulClient, err := consul.NewClient(consul.DefaultConfig())
	if err != nil {
		log.Fatal(err)
	}
	kv := consulClient.KV()

	for _, container := range containers {
		// log.Println(container.Ports)
		var gRPCPort uint16
		for _, portPair := range container.Ports {
			if portPair.PrivatePort == 50052 {
				gRPCPort = portPair.PublicPort
			}
		}

		kvs, _, err := kv.List("instances/"+InstanceID+"/services/"+string(service), &consul.QueryOptions{})
		if err != nil {
			log.Fatal(err)
		}
		configMap, err := consulkvjson.ConsulKVsToJSON(kvs)
		if err != nil {
			log.Fatal(err)
		}

		configString, err := json.Marshal(configMap)
		if err != nil {
			log.Fatal(err)
		}

		serviceConfig, dataType, _, err := jsonparser.Get(configString, "instances", InstanceID, "services", string(service))
		if err != nil {
			log.Fatal(err)
		}
		if dataType == jsonparser.NotExist {
			log.Println(errors.New("invalid JSON"))
		}

		conn, err := grpc.Dial("localhost:"+strconv.Itoa(int(gRPCPort)), grpc.WithInsecure())
		if err != nil {
			log.Fatalf("fail to dial: %v", err)
		}
		defer conn.Close()

		client := managerPb.NewManagerClient(conn)
		_, errConfiguring := client.Configure(ctx, &managerPb.ConfigureRequest{Config: string(serviceConfig)})
		if errConfiguring != nil {
			return errConfiguring
		}

	}
	return nil
}

func configureServices(services Services) []error {
	var errorList []error
	for _, service := range services {
		err := configureService(service)
		if err != nil {
			errorList = append(errorList, err)
		}
	}
	return errorList
}
