package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	ogLog "log"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/imdario/mergo"
	"github.com/jesseduffield/lazydocker/pkg/commands/ssh"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/jesseduffield/lazydocker/pkg/i18n"
	"github.com/jesseduffield/lazydocker/pkg/utils"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
)

const (
	APIVersion = "1.25"
)

// DockerCommand is our main docker interface
type DockerCommand struct {
	Log                    *logrus.Entry
	OSCommand              *OSCommand
	Tr                     *i18n.TranslationSet
	Config                 *config.AppConfig
	Client                 *client.Client
	InDockerComposeProject bool
	ShowExited             bool
	ErrorChan              chan error
	ContainerMutex         sync.Mutex
	ServiceMutex           sync.Mutex

	Containers []*Container
	// DisplayContainers is the array of containers we will display in the containers panel. If Gui.ShowAllContainers is false, this will only be those containers which aren't based on a service. This reduces clutter and duplication in the UI
	DisplayContainers []*Container
	Closers           []io.Closer
}

var _ io.Closer = &DockerCommand{}

// LimitedDockerCommand is a stripped-down DockerCommand with just the methods the container/service/image might need
type LimitedDockerCommand interface {
	NewCommandObject(CommandObject) CommandObject
}

// CommandObject is what we pass to our template resolvers when we are running a custom command. We do not guarantee that all fields will be populated: just the ones that make sense for the current context
type CommandObject struct {
	DockerCompose string
	Service       *Service
	Container     *Container
	Image         *Image
	Volume        *Volume
}

// NewCommandObject takes a command object and returns a default command object with the passed command object merged in
func (c *DockerCommand) NewCommandObject(obj CommandObject) CommandObject {
	defaultObj := CommandObject{DockerCompose: c.Config.UserConfig.CommandTemplates.DockerCompose}
	_ = mergo.Merge(&defaultObj, obj)
	return defaultObj
}

// NewDockerCommand it runs docker commands
func NewDockerCommand(log *logrus.Entry, osCommand *OSCommand, tr *i18n.TranslationSet, config *config.AppConfig, errorChan chan error) (*DockerCommand, error) {
	tunnelCloser, err := ssh.NewSSHHandler(osCommand).HandleSSHDockerHost()
	if err != nil {
		ogLog.Fatal(err)
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion(APIVersion))
	if err != nil {
		ogLog.Fatal(err)
	}

	dockerCommand := &DockerCommand{
		Log:                    log,
		OSCommand:              osCommand,
		Tr:                     tr,
		Config:                 config,
		Client:                 cli,
		ErrorChan:              errorChan,
		ShowExited:             true,
		InDockerComposeProject: true,
		Closers:                []io.Closer{tunnelCloser},
	}

	command := utils.ApplyTemplate(
		config.UserConfig.CommandTemplates.CheckDockerComposeConfig,
		dockerCommand.NewCommandObject(CommandObject{}),
	)

	log.Warn(command)

	err = osCommand.RunCommand(
		utils.ApplyTemplate(
			config.UserConfig.CommandTemplates.CheckDockerComposeConfig,
			dockerCommand.NewCommandObject(CommandObject{}),
		),
	)
	if err != nil {
		dockerCommand.InDockerComposeProject = false
		log.Warn(err.Error())
	}

	return dockerCommand, nil
}

func (c *DockerCommand) Close() error {
	return utils.CloseMany(c.Closers)
}

func (c *DockerCommand) MonitorContainerStats(ctx context.Context) {
	// periodically loop through running containers and see if we need to create a monitor goroutine for any
	// every second we check if we need to spawn a new goroutine
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, container := range c.Containers {
				if !container.MonitoringStats {
					go c.createClientStatMonitor(container)
				}
			}
		}
	}
}

func (c *DockerCommand) createClientStatMonitor(container *Container) {
	container.MonitoringStats = true
	stream, err := c.Client.ContainerStats(context.Background(), container.ID, true)
	if err != nil {
		// not creating error panel because if we've disconnected from docker we'll
		// have already created an error panel
		c.Log.Error(err)
		container.MonitoringStats = false
		return
	}

	defer stream.Body.Close()

	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		data := scanner.Bytes()
		var stats ContainerStats
		_ = json.Unmarshal(data, &stats)

		recordedStats := &RecordedStats{
			ClientStats: stats,
			DerivedStats: DerivedStats{
				CPUPercentage:    stats.CalculateContainerCPUPercentage(),
				MemoryPercentage: stats.CalculateContainerMemoryUsage(),
			},
			RecordedAt: time.Now(),
		}

		container.appendStats(recordedStats)
	}

	container.MonitoringStats = false
}

func (c *DockerCommand) RefreshContainersAndServices(currentServices []*Service) ([]*Container, []*Service, error) {
	c.ServiceMutex.Lock()
	defer c.ServiceMutex.Unlock()

	containers, err := c.GetContainers()
	if err != nil {
		return nil, nil, err
	}

	var services []*Service
	// we only need to get these services once because they won't change in the runtime of the program
	if currentServices != nil {
		services = currentServices
	} else {
		services, err = c.GetServices()
		if err != nil {
			return nil, nil, err
		}
	}

	c.assignContainersToServices(containers, services)

	displayContainers := containers
	if !c.Config.UserConfig.Gui.ShowAllContainers {
		displayContainers = c.obtainStandaloneContainers(containers, services)
	}

	c.Containers = containers
	c.DisplayContainers = c.filterOutExited(displayContainers)
	c.DisplayContainers = c.filterOutIgnoredContainers(c.DisplayContainers)
	c.DisplayContainers = c.sortedContainers(c.DisplayContainers)

	return c.DisplayContainers, services, nil
}

func (c *DockerCommand) assignContainersToServices(containers []*Container, services []*Service) {
L:
	for _, service := range services {
		for _, container := range containers {
			if !container.OneOff && container.ServiceName == service.Name {
				service.Container = container
				continue L
			}
		}
		service.Container = nil
	}
}

// filterOutExited filters out the exited containers if c.ShowExited is false
func (c *DockerCommand) filterOutExited(containers []*Container) []*Container {
	if c.ShowExited {
		return containers
	}
	toReturn := []*Container{}
	for _, container := range containers {
		if container.Container.State != "exited" {
			toReturn = append(toReturn, container)
		}
	}
	return toReturn
}

func (c *DockerCommand) filterOutIgnoredContainers(containers []*Container) []*Container {
	return lo.Filter(containers, func(container *Container, _ int) bool {
		return !lo.SomeBy(c.Config.UserConfig.Ignore, func(ignore string) bool {
			return strings.Contains(container.Name, ignore)
		})
	})
}

// sortedContainers returns containers sorted by state if c.SortContainersByState is true (follows 1- running, 2- exited, 3- created)
// and sorted by name if c.SortContainersByState is false
func (c *DockerCommand) sortedContainers(containers []*Container) []*Container {
	if !c.Config.UserConfig.Gui.LegacySortContainers {
		states := map[string]int{
			"running": 1,
			"exited":  2,
			"created": 3,
		}
		sort.Slice(containers, func(i, j int) bool {
			stateLeft := states[containers[i].Container.State]
			stateRight := states[containers[j].Container.State]
			if stateLeft == stateRight {
				return containers[i].Name < containers[j].Name
			}
			return states[containers[i].Container.State] < states[containers[j].Container.State]
		})
	}
	return containers
}

// obtainStandaloneContainers returns standalone containers. Standalone containers are containers which are either one-off containers, or whose service is not part of this docker-compose context
func (c *DockerCommand) obtainStandaloneContainers(containers []*Container, services []*Service) []*Container {
	standaloneContainers := []*Container{}
L:
	for _, container := range containers {
		for _, service := range services {
			if !container.OneOff && container.ServiceName != "" && container.ServiceName == service.Name {
				continue L
			}
		}
		standaloneContainers = append(standaloneContainers, container)
	}

	return standaloneContainers
}

// GetContainers gets the docker containers
func (c *DockerCommand) GetContainers() ([]*Container, error) {
	c.ContainerMutex.Lock()
	defer c.ContainerMutex.Unlock()

	existingContainers := c.Containers

	containers, err := c.Client.ContainerList(context.Background(), types.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	ownContainers := make([]*Container, len(containers))

	for i, container := range containers {
		var newContainer *Container

		// check if we already data stored against the container
		for _, existingContainer := range existingContainers {
			if existingContainer.ID == container.ID {
				newContainer = existingContainer
				break
			}
		}

		// initialise the container if it's completely new
		if newContainer == nil {
			newContainer = &Container{
				ID:            container.ID,
				Client:        c.Client,
				OSCommand:     c.OSCommand,
				Log:           c.Log,
				Config:        c.Config,
				DockerCommand: c,
				Tr:            c.Tr,
			}
		}

		newContainer.Container = container
		// if the container is made with a name label we will use that
		if name, ok := container.Labels["name"]; ok {
			newContainer.Name = name
		} else {
			newContainer.Name = strings.TrimLeft(container.Names[0], "/")
		}
		newContainer.ServiceName = container.Labels["com.docker.compose.service"]
		newContainer.ProjectName = container.Labels["com.docker.compose.project"]
		newContainer.ContainerNumber = container.Labels["com.docker.compose.container"]
		newContainer.OneOff = container.Labels["com.docker.compose.oneoff"] == "True"

		ownContainers[i] = newContainer
	}

	return ownContainers, nil
}

// GetServices gets services
func (c *DockerCommand) GetServices() ([]*Service, error) {
	if !c.InDockerComposeProject {
		return nil, nil
	}

	composeCommand := c.Config.UserConfig.CommandTemplates.DockerCompose
	output, err := c.OSCommand.RunCommandWithOutput(fmt.Sprintf("%s config --hash=*", composeCommand))
	if err != nil {
		return nil, err
	}

	// output looks like:
	// service1 998d6d286b0499e0ff23d66302e720991a2asdkf9c30d0542034f610daf8a971
	// service2 asdld98asdklasd9bccd02438de0994f8e19cbe691feb3755336ec5ca2c55971

	lines := utils.SplitLines(output)
	services := make([]*Service, len(lines))
	for i, str := range lines {
		arr := strings.Split(str, " ")
		services[i] = &Service{
			Name:          arr[0],
			ID:            arr[1],
			OSCommand:     c.OSCommand,
			Log:           c.Log,
			DockerCommand: c,
		}
	}

	return services, nil
}

// UpdateContainerDetails attaches the details returned from docker inspect to each of the containers
// this contains a bit more info than what you get from the go-docker client
func (c *DockerCommand) UpdateContainerDetails() error {
	c.ContainerMutex.Lock()
	defer c.ContainerMutex.Unlock()

	for _, container := range c.Containers {
		details, err := c.Client.ContainerInspect(context.Background(), container.ID)
		if err != nil {
			c.Log.Error(err)
		} else {
			container.Details = details
		}
	}

	return nil
}

// ViewAllLogs attaches to a subprocess viewing all the logs from docker-compose
func (c *DockerCommand) ViewAllLogs() (*exec.Cmd, error) {
	cmd := c.OSCommand.ExecutableFromString(
		utils.ApplyTemplate(
			c.OSCommand.Config.UserConfig.CommandTemplates.ViewAllLogs,
			c.NewCommandObject(CommandObject{}),
		),
	)

	c.OSCommand.PrepareForChildren(cmd)

	return cmd, nil
}

// DockerComposeConfig returns the result of 'docker-compose config'
func (c *DockerCommand) DockerComposeConfig() string {
	output, err := c.OSCommand.RunCommandWithOutput(
		utils.ApplyTemplate(
			c.OSCommand.Config.UserConfig.CommandTemplates.DockerComposeConfig,
			c.NewCommandObject(CommandObject{}),
		),
	)
	if err != nil {
		output = err.Error()
	}
	return output
}
