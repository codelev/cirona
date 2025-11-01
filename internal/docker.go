package cirona

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// ServiceEvent represents attributes of a service event
type ServiceEvent struct {
	Service     string `mapstructure:"name"`
	UpdateState struct {
		Old string `mapstructure:"updatestate.old"`
		New string `mapstructure:"updatestate.new"`
	} `mapstructure:",squash"`
}

// ServiceListArgs are options to list services
type ServiceListArgs struct {
	Name   string
	Labels []string
}

// ServiceInfo represents attributes of a service
type ServiceInfo struct {
	Raw          swarm.Service
	ID           string
	Name         string
	Image        string
	Mode         ServiceMode
	Labels       map[string]string
	Actives      uint64
	Replicas     uint64
	Rollback     bool
	UpdatedAt    time.Time
	UpdateStatus string
}

// ServiceMode is a service mode
type ServiceMode string

// Client for Swarm
type Client interface {
	DistributionInspect(ctx context.Context, image, encodedAuth string) (registry.DistributionInspect, error)
	RetrieveAuthTokenFromImage(ctx context.Context, image string) (string, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options types.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts types.ServiceInspectOptions) (swarm.Service, []byte, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)

	ServiceList(args *ServiceListArgs) ([]*ServiceInfo, error)
	Service(name string) (*ServiceInfo, error)
	TaskList(service string) ([]*TaskInfo, error)
	Report(ctx context.Context, serviceName string, containerName string, replica string) error
}

type DockerClient struct {
	api *client.Client
	cli command.Cli
}

// Service modes available
const (
	ServiceModeReplicated = ServiceMode("replicated")
	ServiceModeGlobal     = ServiceMode("global")
)

// TaskInfo represents attributes of a task
type TaskInfo struct {
	swarm.Task
	NodeName    string
	ServiceName string
	Image       string
}

// ServiceList return all services.
func (c *DockerClient) ServiceList(args *ServiceListArgs) ([]*ServiceInfo, error) {
	opts := types.ServiceListOptions{
		Filters: filters.NewArgs(),
	}
	if args.Name != "" {
		opts.Filters.Add("name", args.Name)
	}
	if len(args.Labels) > 0 {
		for _, label := range args.Labels {
			opts.Filters.Add("label", label)
		}
	}

	services, err := c.api.ServiceList(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	sort.Slice(services, func(i, j int) bool {
		return services[i].Spec.Name < services[j].Spec.Name
	})

	// nodes
	nodes, err := c.api.NodeList(context.Background(), types.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	activeNodes := make(map[string]struct{})
	for _, node := range nodes {
		if node.Status.State != swarm.NodeStateDown {
			activeNodes[node.ID] = struct{}{}
		}
	}

	// tasks
	taskOpts := types.TaskListOptions{
		Filters: filters.NewArgs(),
	}
	for _, service := range services {
		taskOpts.Filters.Add("service", service.ID)
	}
	tasks, err := c.api.TaskList(context.Background(), taskOpts)
	if err != nil {
		return nil, err
	}

	// active tasks
	running, tasksNoShutdown := map[string]uint64{}, map[string]uint64{}
	for _, task := range tasks {
		if task.DesiredState != swarm.TaskStateShutdown {
			tasksNoShutdown[task.ServiceID]++
		}
		if _, nodeActive := activeNodes[task.NodeID]; nodeActive && task.Status.State == swarm.TaskStateRunning {
			running[task.ServiceID]++
		}
	}

	// res
	res := make([]*ServiceInfo, len(services))
	for i, service := range services {
		res[i] = &ServiceInfo{
			Raw:       service,
			ID:        service.ID,
			Name:      service.Spec.Name,
			Image:     normalizeImage(service.Spec.TaskTemplate.ContainerSpec.Image),
			Labels:    service.Spec.Labels,
			Actives:   running[service.ID],
			UpdatedAt: service.UpdatedAt.Local(),
			Rollback:  service.PreviousSpec != nil,
		}
		if service.UpdateStatus != nil {
			res[i].UpdateStatus = string(service.UpdateStatus.State)
		}
		if service.Spec.Mode.Replicated != nil && service.Spec.Mode.Replicated.Replicas != nil {
			res[i].Mode = ServiceModeReplicated
			res[i].Replicas = *service.Spec.Mode.Replicated.Replicas
		} else if service.Spec.Mode.Global != nil {
			res[i].Mode = ServiceModeGlobal
			res[i].Replicas = tasksNoShutdown[service.ID]
		}
	}

	return res, nil
}

// Service returns a service
func (c *DockerClient) Service(name string) (*ServiceInfo, error) {
	service, err := c.ServiceList(&ServiceListArgs{
		Name: name,
	})
	if err != nil {
		return nil, err
	} else if len(service) == 0 {
		return nil, errors.Errorf("%s service not found", name)
	}

	return service[0], nil
}

// NewDockerClient initializes a new Docker API client based on environment variables
func NewDockerClient() (*DockerClient, error) {
	dockerAPICli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize Docker API client")
	}

	_, err = dockerAPICli.ServerVersion(context.Background())
	if err != nil {
		return nil, err
	}

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Docker cli")
	}

	return &DockerClient{
		api: dockerAPICli,
		cli: dockerCli,
	}, err
}

// DistributionInspect returns the image digest with full Manifest
func (c *DockerClient) DistributionInspect(ctx context.Context, image, encodedAuth string) (registry.DistributionInspect, error) {
	return c.api.DistributionInspect(ctx, image, encodedAuth)
}

// RetrieveAuthTokenFromImage retrieves an encoded auth token given a complete image
func (c *DockerClient) RetrieveAuthTokenFromImage(ctx context.Context, image string) (string, error) {
	return retrieveAuthTokenFromImage(ctx, c.cli, image)
}

// ServiceUpdate updates a Service. The version number is required to avoid conflicting writes.
// It should be the value as set *before* the update. You can find this value in the Meta field
// of swarm.Service, which can be found using ServiceInspectWithRaw.
func (c *DockerClient) ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options types.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	return c.api.ServiceUpdate(ctx, serviceID, version, service, options)
}

// ServiceInspectWithRaw returns the service information and the raw data.
func (c *DockerClient) ServiceInspectWithRaw(ctx context.Context, serviceID string, opts types.ServiceInspectOptions) (swarm.Service, []byte, error) {
	return c.api.ServiceInspectWithRaw(ctx, serviceID, opts)
}

// Events returns a stream of events in the daemon. It's up to the caller to close the stream
// by cancelling the context. Once the stream has been completely read an io.EOF error will
// be sent over the error channel. If an error is sent all processing will be stopped. It's up
// to the caller to reopen the stream in the event of an error by reinvoking this method.
func (c *DockerClient) Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error) {
	return c.api.Events(ctx, options)
}

// Report reads and reports container logs and state
func (c *DockerClient) Report(ctx context.Context, serviceName string, containerName string, replica string) error {
	inspect, err := c.api.ContainerInspect(ctx, containerName)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil
		}
		return err
	}
	if inspect.State.Status != container.StateExited {
		return nil
	}

	reader, err := c.api.ContainerLogs(ctx, containerName, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "1",
		Follow:     false,
		Timestamps: false,
	})
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil
		}
		log.Error().Str("container", containerName).Err(err).Msg("Failed to read logs")
		return err
	}
	defer reader.Close()

	var stdout, stderr strings.Builder
	_, err = stdcopy.StdCopy(&stdout, &stderr, reader)
	if err != nil {
		return err
	}

	stdoutStr := strings.TrimSpace(stdout.String())
	stderrStr := strings.TrimSpace(stderr.String())

	startedAt, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
	if err != nil {
		return err
	}
	finishedAt, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt)
	if err != nil {
		return err
	}
	duration := finishedAt.Sub(startedAt).Milliseconds()

	log.Info().
		Str("service", serviceName).
		Str("container", containerName).
		Str("replica", replica).
		Int("exitCode", inspect.State.ExitCode).
		Str("stdout", stdoutStr).
		Str("stderr", stderrStr).
		Str("stderr", stderrStr).
		Str("duration", strconv.FormatInt(duration, 10)).
		Msg("Finish the job")
	MeterJobExecution(serviceName, replica, inspect.State.ExitCode, stdoutStr, stderrStr, duration)

	return nil
}

func normalizeImage(image string) string {
	if i := strings.Index(image, "@sha256:"); i > 0 {
		image = image[:i]
	}
	return image
}
