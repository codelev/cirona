package cirona

import (
	"context"
	"sort"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/rs/zerolog/log"
)

// Job holds service Job details
type Job struct {
	Name          string
	Enable        bool
	Schedule      string
	SkipRunning   bool
	RegistryAuth  bool
	QueryRegistry *bool
	Replicas      uint64
}

// WorkerClient Client represents an active worker object
type WorkerClient struct {
	Docker Client
	Job    Job
}

// Run runs a Cron based service
func (c *WorkerClient) Run() {
	service, err := c.Docker.Service(c.Job.Name)
	if err != nil {
		log.Error().Str("service", c.Job.Name).Err(err).Msg("Service not found")
		return
	}
	serviceUp := service.Raw

	tasks, err := c.Docker.TaskList(c.Job.Name)
	if err != nil {
		log.Error().Str("service", c.Job.Name).Err(err).Msg("Cannot find job in the service")
		return
	}
	for _, task := range tasks {
		log.Debug().
			Str("node", task.NodeName).
			Str("service", task.ServiceName).
			Str("task_id", task.ID).
			Str("status_message", task.Status.Message).
			Str("status_state", string(task.Status.State)).
			Msg("Service task")
	}

	if c.Job.SkipRunning && service.Actives > 0 {
		log.Warn().Str("service", c.Job.Name).Uint64("tasks_active", service.Actives).Msg("Skip running Job")
		return
	}

	log.Info().
		Str("service", c.Job.Name).
		Uint64("tasks_active", service.Actives).
		Str("status", service.UpdateStatus).
		Msg("Start the job")
	MeterJobExecutionsTotal(c.Job.Name, c.Job.Schedule, c.Job.Replicas)

	// Set number of replicas in replicated mode
	if service.Mode == ServiceModeReplicated {
		if c.Job.Replicas > 1 {
			// Need to scale down service to 0 to fix an issue if replicas > 1
			// See https://github.com/crazy-max/cirona/issues/16
			if serviceUp, err = c.scaleDown(serviceUp); err != nil {
				log.Error().Str("service", c.Job.Name).Err(err).Msg("Cannot scaled down the service")
			}
		}
		*serviceUp.Spec.Mode.Replicated.Replicas = c.Job.Replicas
	}

	// Set ForceUpdate with Version to ensure update
	serviceUp.Spec.TaskTemplate.ForceUpdate = serviceUp.Version.Index

	// Update options
	updateOpts := types.ServiceUpdateOptions{}
	if c.Job.RegistryAuth {
		encodedAuth, err := c.Docker.RetrieveAuthTokenFromImage(context.Background(), serviceUp.Spec.TaskTemplate.ContainerSpec.Image)
		if err != nil {
			log.Error().Str("service", c.Job.Name).Err(err).Msg("Cannot retrieve auth token from the image")
			return
		}
		if encodedAuth != "e30=" {
			updateOpts.EncodedRegistryAuth = encodedAuth
		}
	} else {
		updateOpts.RegistryAuthFrom = types.RegistryAuthFromSpec
	}
	if c.Job.QueryRegistry != nil {
		updateOpts.QueryRegistry = *c.Job.QueryRegistry
	}

	// Update service
	response, err := c.Docker.ServiceUpdate(context.Background(), serviceUp.ID, serviceUp.Version, serviceUp.Spec, updateOpts)
	if err != nil {
		log.Error().Str("service", c.Job.Name).Err(err).Msg("Cannot update the service")
	}
	for _, warn := range response.Warnings {
		log.Warn().Str("service", c.Job.Name).Msg(warn)
	}
}

// scaleDown decreases number of replicas of a service
func (c *WorkerClient) scaleDown(serviceRaw swarm.Service) (swarm.Service, error) {
	*serviceRaw.Spec.Mode.Replicated.Replicas = 0
	serviceRaw.Spec.Labels["cirona.scaledown"] = "true"
	serviceRaw.Spec.TaskTemplate.ForceUpdate = serviceRaw.Version.Index

	_, err := c.Docker.ServiceUpdate(context.Background(), serviceRaw.ID, serviceRaw.Version, serviceRaw.Spec, types.ServiceUpdateOptions{})
	if err != nil {
		return swarm.Service{}, err
	}

	service, err := c.Docker.Service(c.Job.Name)
	if err != nil {
		return swarm.Service{}, err
	}

	delete(service.Raw.Spec.Labels, "cirona.scaledown")
	return service.Raw, nil
}

// TaskList returns all running tasks of a service
func (c *DockerClient) TaskList(service string) ([]*TaskInfo, error) {
	tasksFilters := filters.NewArgs()
	tasksFilters.Add("service", service)
	tasks, err := c.api.TaskList(context.Background(), types.TaskListOptions{
		Filters: tasksFilters,
	})
	if err != nil || len(tasks) == 0 {
		return nil, err
	}

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})
	nodes := make(map[string]string)
	for _, t := range tasks {
		if _, ok := nodes[t.NodeID]; !ok {
			if node, _, e := c.api.NodeInspectWithRaw(context.Background(), t.NodeID); e == nil {
				if node.Spec.Name == "" {
					nodes[t.NodeID] = node.Description.Hostname
				} else {
					nodes[t.NodeID] = node.Spec.Name
				}
			} else {
				nodes[t.NodeID] = ""
			}
		}
	}

	res := make([]*TaskInfo, len(tasks))
	for i, t := range tasks {
		res[i] = &TaskInfo{
			Task:        t,
			NodeName:    nodes[t.NodeID],
			ServiceName: service,
			Image:       normalizeImage(t.Spec.ContainerSpec.Image),
		}
	}

	return res, nil
}
