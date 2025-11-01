package cirona

import (
	"context"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/mitchellh/mapstructure"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog/log"
)

// Scheduler represents an object
type Scheduler struct {
	Docker Client
	Cron   *cron.Cron
	Jobs   map[string]cron.EntryID
}

// NewScheduler creates instance
func NewScheduler() (*Scheduler, error) {
	log.Debug().Msg("Creating Docker client...")
	dockerClient, err := NewDockerClient()

	return &Scheduler{
		Docker: dockerClient,
		Cron: cron.New(cron.WithParser(cron.NewParser(
			cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		)),
		Jobs: make(map[string]cron.EntryID),
	}, err
}

// RunScheduler Starts process
func (c *Scheduler) RunScheduler() error {
	// Find services
	services, err := c.Docker.ServiceList(&ServiceListArgs{
		Labels: []string{
			"cirona.enable",
			"cirona.schedule",
		},
	})
	if err != nil {
		return err
	}
	log.Debug().Msgf("Services found: %d", len(services))

	// Add services as Jobs
	for _, service := range services {
		if _, err := c.crudJob(service.Name); err != nil {
			log.Error().Str("service", service.Name).Err(err).Msg("Cannot manage job for the service")
		}
	}

	// Start Cron routine
	log.Debug().Msg("Starting cron...")
	c.Cron.Start()

	// Listen Docker events
	log.Debug().Msg("Listening to Docker events...")
	filter := filters.NewArgs()
	filter.Add("type", string(events.ServiceEventType))
	filter.Add("type", string(events.ContainerEventType))
	ctx := context.Background()
	msgs, errs := c.Docker.Events(ctx, events.ListOptions{
		Filters: filter,
	})

	var event ServiceEvent
	for {
		select {
		case err := <-errs:
			log.Fatal().Err(err).Msg("Docker events failed")
		case msg := <-msgs:
			err := mapstructure.Decode(msg.Actor.Attributes, &event)
			if err != nil {
				log.Error().Str("service", event.Service).Err(err).Msg("Cannot decode the event")
				continue
			}
			log.Debug().
				Str("service", event.Service).
				Str("newstate", event.UpdateState.New).
				Str("oldstate", event.UpdateState.Old).
				Msg("Event detected")
			if msg.Type == events.ServiceEventType {
				processed, err := c.crudJob(event.Service)
				if err != nil {
					log.Error().Str("service", event.Service).Err(err).Msg("Cannot manage the service")
					continue
				} else if processed {
					log.Debug().Msgf("Jobs found: %d", len(c.Cron.Entries()))
				}
			}
			if msg.Type == events.ContainerEventType {
				err = c.inspectJob(ctx, event.Service)
				if err != nil {
					log.Error().Str("service", event.Service).Err(err).Msg("Cannot report the service jobs")
				}
			}
		}
	}
}

// crudJob adds, updates or removes service
func (c *Scheduler) crudJob(serviceName string) (bool, error) {
	// Find existing Job
	jobID, jobFound := c.Jobs[serviceName]

	// Check service exists
	service, err := c.Docker.Service(serviceName)
	if err != nil {
		if jobFound {
			log.Info().Str("service", serviceName).Msg("Remove the service")
			c.removeJob(serviceName, jobID)
			return true, nil
		}
		log.Debug().Str("service", serviceName).Msg("Service does not exist (removed)")
		return false, nil
	}

	// Worker
	wc := &WorkerClient{
		Docker: c.Docker,
		Job: Job{
			Name:        service.Name,
			Enable:      false,
			SkipRunning: false,
			Replicas:    1,
		},
	}

	// Seek labels
	for labelKey, labelValue := range service.Labels {
		switch labelKey {
		case "cirona.enable":
			wc.Job.Enable, err = strconv.ParseBool(labelValue)
			if err != nil {
				log.Error().Str("service", service.Name).Err(err).Msgf("Cannot parse %s value of label %s", labelValue, labelKey)
			}
		case "cirona.schedule":
			wc.Job.Schedule = labelValue
		case "cirona.skip-running":
			wc.Job.SkipRunning, err = strconv.ParseBool(labelValue)
			if err != nil {
				log.Error().Str("service", service.Name).Err(err).Msgf("Cannot parse %s value of label %s", labelValue, labelKey)
			}
		case "cirona.replicas":
			wc.Job.Replicas, err = strconv.ParseUint(labelValue, 10, 64)
			if err != nil {
				log.Error().Str("service", service.Name).Err(err).Msgf("Cannot parse %s value of label %s", labelValue, labelKey)
			} else if wc.Job.Replicas < 1 {
				log.Error().Str("service", service.Name).Msgf("%s must be greater than or equal to one", labelKey)
			}
		case "cirona.registry-auth":
			wc.Job.RegistryAuth, err = strconv.ParseBool(labelValue)
			if err != nil {
				log.Error().Str("service", service.Name).Err(err).Msgf("Cannot parse %s value of label %s", labelValue, labelKey)
			}
		case "cirona.query-registry":
			queryRegistry, err := strconv.ParseBool(labelValue)
			if err != nil {
				log.Error().Str("service", service.Name).Err(err).Msgf("Cannot parse %s value of label %s", labelValue, labelKey)
			}
			wc.Job.QueryRegistry = &queryRegistry
		case "cirona.scaledown":
			if labelValue == "true" {
				log.Debug().Str("service", service.Name).Msg("Downscale detected, skipping the job")
				return false, nil
			}
		}
	}

	// Disabled or non-Cron service
	if !wc.Job.Enable {
		if jobFound {
			log.Info().Str("service", service.Name).Msg("Disable the service")
			c.removeJob(serviceName, jobID)
			return true, nil
		}
		return false, nil
	}

	// Add/Update Job
	if jobFound {
		c.removeJob(serviceName, jobID)
		log.Info().Str("service", service.Name).Str("schedule", wc.Job.Schedule).Msg("Update the service")
	} else {
		log.Info().Str("service", service.Name).Str("schedule", wc.Job.Schedule).Msg("Add the service")
	}

	jobID, err = c.Cron.AddJob(wc.Job.Schedule, wc)
	if err != nil {
		return false, err
	}

	c.Jobs[serviceName] = jobID
	MeterServiceStatus(serviceName, true)
	return true, err
}

// CloseScheduler closes instance
func (c *Scheduler) CloseScheduler() {
	if c.Cron != nil {
		c.Cron.Stop()
	}
}

// removeJob removes Job
func (c *Scheduler) removeJob(serviceName string, id cron.EntryID) {
	delete(c.Jobs, serviceName)
	c.Cron.Remove(id)
	MeterServiceStatus(serviceName, false)
}

// reportJob adds, updates or removes service
func (c *Scheduler) inspectJob(ctx context.Context, containerName string) error {
	// Job exists?
	serviceName, replica := getServiceAndReplica(containerName)
	_, jobFound := c.Jobs[serviceName]
	if !jobFound {
		return nil
	}

	return c.Docker.Report(ctx, serviceName, containerName, replica)
}

func getServiceAndReplica(containerName string) (service string, replica string) {
	parts := strings.Split(containerName, ".")
	if len(parts) < 2 {
		return containerName, ""
	}
	return parts[0], parts[1]
}
