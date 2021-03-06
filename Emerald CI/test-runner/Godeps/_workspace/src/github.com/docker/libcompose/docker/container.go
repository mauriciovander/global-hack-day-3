package docker

import (
	"bufio"
	"fmt"
	"math"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/graph/tags"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/utils"
	"github.com/docker/libcompose/logger"
	"github.com/docker/libcompose/project"
	"github.com/samalba/dockerclient"
)

type Container struct {
	project.EmptyService

	name    string
	service *Service
	client  dockerclient.Client
}

func NewContainer(client dockerclient.Client, name string, service *Service) *Container {
	return &Container{
		client:  client,
		name:    name,
		service: service,
	}
}

func (c *Container) findExisting() (*dockerclient.Container, error) {
	return GetContainerByName(c.client, c.name)
}

func (c *Container) findInfo() (*dockerclient.ContainerInfo, error) {
	container, err := c.findExisting()
	if err != nil {
		return nil, err
	}

	return c.client.InspectContainer(container.Id)
}

func (c *Container) Info() (project.Info, error) {
	container, err := c.findExisting()
	if err != nil {
		return nil, err
	}

	result := project.Info{}

	result = append(result, project.InfoPart{Key: "Name", Value: name(container.Names)})
	result = append(result, project.InfoPart{Key: "Command", Value: container.Command})
	result = append(result, project.InfoPart{Key: "State", Value: container.Status})
	result = append(result, project.InfoPart{Key: "Ports", Value: portString(container.Ports)})

	return result, nil
}

func portString(ports []dockerclient.Port) string {
	result := []string{}

	for _, port := range ports {
		if port.PublicPort > 0 {
			result = append(result, fmt.Sprintf("%s:%d->%d/%s", port.IP, port.PublicPort, port.PrivatePort, port.Type))
		} else {
			result = append(result, fmt.Sprintf("%d/%s", port.PrivatePort, port.Type))
		}
	}

	return strings.Join(result, ", ")
}

func name(names []string) string {
	max := math.MaxInt32
	var current string

	for _, v := range names {
		if len(v) < max {
			max = len(v)
			current = v
		}
	}

	return current[1:]
}

func (c *Container) Create(imageName string) (*dockerclient.Container, error) {
	return c.CreateWithOverride(imageName, nil)
}

func (c *Container) CreateWithOverride(imageName string, configOverride *project.ServiceConfig) (*dockerclient.Container, error) {
	container, err := c.findExisting()
	if err != nil {
		return nil, err
	}

	if container == nil {
		container, err = c.createContainer(imageName, configOverride)
		if err != nil {
			return nil, err
		}
		c.service.context.Project.Notify(project.CONTAINER_CREATED, c.service.Name(), map[string]string{
			"name": c.Name(),
		})
	}

	return container, err
}

func (c *Container) Down() error {
	return c.withContainer(func(container *dockerclient.Container) error {
		return c.client.StopContainer(container.Id, c.service.context.Timeout)
	})
}

func (c *Container) Kill() error {
	return c.withContainer(func(container *dockerclient.Container) error {
		return c.client.KillContainer(container.Id, c.service.context.Signal)
	})
}

func (c *Container) Delete() error {
	container, err := c.findExisting()
	if err != nil || container == nil {
		return err
	}

	info, err := c.client.InspectContainer(container.Id)
	if err != nil {
		return err
	}

	if info.State.Running {
		err := c.client.StopContainer(container.Id, c.service.context.Timeout)
		if err != nil {
			return err
		}
	}

	return c.client.RemoveContainer(container.Id, true, false)
}

func (c *Container) Up(imageName string) error {
	var err error

	defer func() {
		if err == nil && c.service.context.Log {
			go c.Log()
		}
	}()

	container, err := c.Create(imageName)
	if err != nil {
		return err
	}

	info, err := c.client.InspectContainer(container.Id)
	if err != nil {
		return err
	}

	if !info.State.Running {
		err := c.Start(container, info.HostConfig)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Container) Start(container *dockerclient.Container, hostConfig *dockerclient.HostConfig) error {
	logrus.Debugf("Starting container: %s: %#v", container.Id, hostConfig)
	err := c.populateAdditionalHostConfig(hostConfig)
	if err != nil {
		return err
	}

	if err = c.client.StartContainer(container.Id, hostConfig); err != nil {
		return err
	}

	c.service.context.Project.Notify(project.CONTAINER_STARTED, c.service.Name(), map[string]string{
		"name": c.Name(),
	})

	return nil
}

func (c *Container) OutOfSync() (bool, error) {
	container, err := c.findExisting()
	if err != nil || container == nil {
		return false, err
	}

	info, err := c.client.InspectContainer(container.Id)
	if err != nil {
		return false, err
	}

	return info.Config.Labels[HASH.Str()] != project.GetServiceHash(c.service), nil
}

func (c *Container) createContainer(imageName string, configOverride *project.ServiceConfig) (*dockerclient.Container, error) {
	serviceConfig := c.service.serviceConfig
	if configOverride != nil {
		serviceConfig.Command = configOverride.Command
		serviceConfig.Tty = configOverride.Tty
	}
	config, err := ConvertToApi(serviceConfig)
	if err != nil {
		return nil, err
	}

	config.Image = imageName

	if config.Labels == nil {
		config.Labels = map[string]string{}
	}

	config.Labels[NAME.Str()] = c.name
	config.Labels[SERVICE.Str()] = c.service.name
	config.Labels[PROJECT.Str()] = c.service.context.Project.Name
	config.Labels[HASH.Str()] = project.GetServiceHash(c.service)

	err = c.populateAdditionalHostConfig(&config.HostConfig)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("Creating container %s %#v", c.name, config)

	_, err = c.client.CreateContainer(config, c.name)
	if err != nil && err.Error() == "Not found" {
		logrus.Debugf("Not Found, pulling image %s", config.Image)
		if err = c.pull(config.Image); err != nil {
			return nil, err
		}
		if _, err = c.client.CreateContainer(config, c.name); err != nil {
			return nil, err
		}
	}

	if err != nil {
		logrus.Debugf("Failed to create container %s: %v", c.name, err)
		return nil, err
	}

	return c.findExisting()
}

func (c *Container) populateAdditionalHostConfig(hostConfig *dockerclient.HostConfig) error {
	links := map[string]string{}

	for _, link := range c.service.DependentServices() {
		if _, ok := c.service.context.Project.Configs[link.Target]; !ok {
			continue
		}

		service, err := c.service.context.Project.CreateService(link.Target)
		if err != nil {
			return err
		}

		containers, err := service.Containers()
		if err != nil {
			return err
		}

		if link.Type == project.REL_TYPE_LINK {
			c.addLinks(links, service, link, containers)
		} else if link.Type == project.REL_TYPE_IPC_NAMESPACE {
			hostConfig, err = c.addIpc(hostConfig, service, containers)
		} else if link.Type == project.REL_TYPE_NET_NAMESPACE {
			hostConfig, err = c.addNetNs(hostConfig, service, containers)
		}

		if err != nil {
			return err
		}
	}

	hostConfig.Links = []string{}
	for k, v := range links {
		hostConfig.Links = append(hostConfig.Links, strings.Join([]string{v, k}, ":"))
	}
	for _, v := range c.service.Config().ExternalLinks {
		hostConfig.Links = append(hostConfig.Links, v)
	}

	return nil
}

func (c *Container) addLinks(links map[string]string, service project.Service, rel project.ServiceRelationship, containers []project.Container) {
	for _, container := range containers {
		if _, ok := links[rel.Alias]; !ok {
			links[rel.Alias] = container.Name()
		}

		links[container.Name()] = container.Name()
	}
}

func (c *Container) addIpc(config *dockerclient.HostConfig, service project.Service, containers []project.Container) (*dockerclient.HostConfig, error) {
	if len(containers) == 0 {
		return nil, fmt.Errorf("Failed to find container for IPC %v", c.service.Config().Ipc)
	}

	id, err := containers[0].Id()
	if err != nil {
		return nil, err
	}

	config.IpcMode = "container:" + id
	return config, nil
}

func (c *Container) addNetNs(config *dockerclient.HostConfig, service project.Service, containers []project.Container) (*dockerclient.HostConfig, error) {
	if len(containers) == 0 {
		return nil, fmt.Errorf("Failed to find container for networks ns %v", c.service.Config().Net)
	}

	id, err := containers[0].Id()
	if err != nil {
		return nil, err
	}

	config.NetworkMode = "container:" + id
	return config, nil
}

func (c *Container) Id() (string, error) {
	container, err := c.findExisting()
	if container == nil {
		return "", err
	} else {
		return container.Id, err
	}
}

func (c *Container) Name() string {
	return c.name
}

func (c *Container) Pull() error {
	return c.pull(c.service.serviceConfig.Image)
}

func (c *Container) Restart() error {
	container, err := c.findExisting()
	if err != nil || container == nil {
		return err
	}

	return c.client.RestartContainer(container.Id, c.service.context.Timeout)
}

func (c *Container) Log() error {
	container, err := c.findExisting()
	if container == nil || err != nil {
		return err
	}

	info, err := c.client.InspectContainer(container.Id)
	if info == nil || err != nil {
		return err
	}

	l := c.service.context.LoggerFactory.Create(c.name)

	output, err := c.client.ContainerLogs(container.Id, &dockerclient.LogOptions{
		Follow: true,
		Stdout: true,
		Stderr: true,
		Tail:   10,
	})
	if err != nil {
		return err
	}

	if info.Config.Tty {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			l.Out([]byte(scanner.Text() + "\n"))
		}
		return scanner.Err()
	} else {
		_, err := stdcopy.StdCopy(&logger.LoggerWrapper{
			Logger: l,
		}, &logger.LoggerWrapper{
			Err:    true,
			Logger: l,
		}, output)
		return err
	}
}

func (c *Container) pull(image string) error {
	taglessRemote, tag := parsers.ParseRepositoryTag(image)
	if tag == "" {
		image = utils.ImageReference(taglessRemote, tags.DEFAULTTAG)
	}

	repoInfo, err := registry.ParseRepositoryInfo(taglessRemote)
	if err != nil {
		return err
	}

	authConfig := cliconfig.AuthConfig{}
	if c.service.context.ConfigFile != nil && repoInfo != nil && repoInfo.Index != nil {
		authConfig = registry.ResolveAuthConfig(c.service.context.ConfigFile, repoInfo.Index)
	}

	err = c.client.PullImage(image, &dockerclient.AuthConfig{
		Username: authConfig.Username,
		Password: authConfig.Password,
		Email:    authConfig.Email,
	})

	if err != nil {
		logrus.Errorf("Failed to pull image %s: %v", image, err)
	}

	return err
}

func (c *Container) withContainer(action func(*dockerclient.Container) error) error {
	container, err := c.findExisting()
	if err != nil {
		return err
	}

	if container != nil {
		return action(container)
	}

	return nil
}

func (c *Container) Port(port string) (string, error) {
	info, err := c.findInfo()
	if err != nil {
		return "", err
	}

	if bindings, ok := info.NetworkSettings.Ports[port]; ok {
		result := []string{}
		for _, binding := range bindings {
			result = append(result, binding.HostIp+":"+binding.HostPort)
		}

		return strings.Join(result, "\n"), nil
	} else {
		return "", nil
	}
}
