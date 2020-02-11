package shipyard

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/mitchellh/mapstructure"
	"github.com/shipyard-run/shipyard/pkg/clients"
	"github.com/shipyard-run/shipyard/pkg/config"
	"github.com/shipyard-run/shipyard/pkg/providers"
	"github.com/shipyard-run/shipyard/pkg/utils"
)

// Clients contains clients which are responsible for creating and destrying reources
type Clients struct {
	Docker         clients.Docker
	ContainerTasks clients.ContainerTasks
	Kubernetes     clients.Kubernetes
	HTTP           clients.HTTP
	Command        clients.Command
}

// Engine is responsible for creating and destroying resources
type Engine struct {
	providers         [][]providers.Provider
	clients           *Clients
	config            *config.Config
	log               hclog.Logger
	generateProviders generateProvidersFunc
	stateLock         sync.Mutex
	state             []providers.ConfigWrapper
}

type generateProvidersFunc func(c *config.Config, cl *Clients, l hclog.Logger) [][]providers.Provider

// GenerateClients creates the various clients for creating and destroying resources
func GenerateClients(l hclog.Logger) (*Clients, error) {
	dc, err := clients.NewDocker()
	if err != nil {
		return nil, err
	}

	kc := clients.NewKubernetes(60 * time.Second)

	ec := clients.NewCommand(30*time.Second, l)

	ct := clients.NewDockerTasks(dc, l)

	hc := clients.NewHTTP(1*time.Second, l)

	return &Clients{
		ContainerTasks: ct,
		Docker:         dc,
		Kubernetes:     kc,
		Command:        ec,
		HTTP:           hc,
	}, nil
}

// NewWithFolder creates a new shipyard engine with a given configuration folder
func NewWithFolder(folder string, l hclog.Logger) (*Engine, error) {
	var err error

	cc, err := config.New()
	if err != nil {
		return nil, err
	}

	err = config.ParseFolder(folder, cc)
	if err != nil {
		return nil, err
	}

	err = config.ParseReferences(cc)
	if err != nil {
		return nil, err
	}

	// create providers
	cl, err := GenerateClients(l)
	if err != nil {
		return nil, err
	}

	e := New(cc, cl, l)

	return e, nil
}

// NewFromState creates an engine from the statefile rather than the provided blueprint
func NewFromState(statePath string, l hclog.Logger) (*Engine, error) {
	cc, err := configFromState(statePath)
	if err != nil {
		return nil, err
	}

	err = config.ParseReferences(cc)
	if err != nil {
		return nil, err
	}

	// create providers
	cl, err := GenerateClients(l)
	if err != nil {
		return nil, err
	}

	e := New(cc, cl, l)

	return e, nil
}

func configFromState(path string) (*config.Config, error) {
	cc, err := config.New()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := []interface{}{}
	jd := json.NewDecoder(f)
	jd.Decode(&s)

	// for each item set the config
	for _, c := range s {
		switch c.(map[string]interface{})["Type"].(string) {
		case "config.Network":

			n := &config.Network{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			// do not add the wan as this is automatically created
			if n.Name != "wan" {
				cc.Networks = append(cc.Networks, n)
			}
		case "config.Docs":
			n := &config.Docs{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.Docs = n
		case "config.Cluster":
			n := &config.Cluster{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.Clusters = append(cc.Clusters, n)
		case "config.Container":
			n := &config.Container{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.Containers = append(cc.Containers, n)
		case "config.Helm":
			n := &config.Helm{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.HelmCharts = append(cc.HelmCharts, n)
		case "config.K8sConfig":
			n := &config.K8sConfig{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.K8sConfig = append(cc.K8sConfig, n)
		case "config.Ingress":
			n := &config.Ingress{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.Ingresses = append(cc.Ingresses, n)
		case "config.LocalExec":
			n := &config.LocalExec{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.LocalExecs = append(cc.LocalExecs, n)
		case "config.RemoteExec":
			n := &config.RemoteExec{}
			err := mapstructure.Decode(c.(map[string]interface{})["Value"].(interface{}), &n)
			if err != nil {
				return nil, err
			}

			cc.RemoteExecs = append(cc.RemoteExecs, n)
		}
	}

	return cc, nil
}

// New engine using the given configuration and clients
func New(c *config.Config, cc *Clients, l hclog.Logger) *Engine {

	e := &Engine{
		clients:           cc,
		config:            c,
		log:               l,
		generateProviders: generateProvidersImpl,
		stateLock:         sync.Mutex{},
	}

	p := e.generateProviders(c, cc, l)
	e.providers = p

	return e
}

// Apply the current config creating the resources
func (e *Engine) Apply() error {

	var err error
	// loop through each group
	for _, g := range e.providers {
		// apply the provider in parallel
		createErr := e.createParallel(g)
		if createErr != nil {
			err = createErr
			break
		}
	}

	// save the state regardless of error
	e.saveState()

	return err
}

// Destroy the resources defined by the config
func (e *Engine) Destroy() error {
	// should run through the providers in reverse order
	// to ensure objects with dependencies are destroyed first
	for i := len(e.providers) - 1; i > -1; i-- {

		err := e.destroyParallel(e.providers[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// ResourceCount defines the number of resources in a plan
func (e *Engine) ResourceCount() int {
	return e.config.ResourceCount()
}

// Blueprint returns the blueprint for the current config
func (e *Engine) Blueprint() *config.Blueprint {
	return e.config.Blueprint
}

// createParallel is just a quick implementation for now to test the UX
func (e *Engine) createParallel(p []providers.Provider) error {
	errs := make(chan error)
	done := make(chan struct{})

	// create the wait group and set the size to the provider length
	wg := sync.WaitGroup{}
	wg.Add(len(p))

	for _, pr := range p {
		go func(pr providers.Provider) {
			err := pr.Create()
			if err != nil {
				errs <- err
			}

			// append the state
			e.stateLock.Lock()
			defer e.stateLock.Unlock()
			e.state = append(e.state, pr.Config())

			wg.Done()
		}(pr)
	}

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-done:
		return nil
	case err := <-errs:
		return err
	}

}

// destroyParallel is just a quick implementation for now to test the UX
func (e *Engine) destroyParallel(p []providers.Provider) error {
	// create the wait group and set the size to the provider length
	wg := sync.WaitGroup{}
	wg.Add(len(p))

	for _, pr := range p {
		go func(pr providers.Provider) {
			pr.Destroy()
			wg.Done()
		}(pr)
	}

	wg.Wait()

	return nil
}

// save state serializes the state file into json formatted file
func (e *Engine) saveState() error {
	e.log.Info("Writing state file")

	sd := utils.StateDir()
	sp := utils.StatePath()

	// if it does not exist create the state folder
	_, err := os.Stat(sd)
	if err != nil {
		os.MkdirAll(sd, os.ModePerm)
	}

	// if the statefile exists overwrite it
	_, err = os.Stat(sp)
	if err == nil {
		// delete the old state
		os.Remove(sp)
	}

	// serialize the state to json and write to a file
	f, err := os.Create(sp)
	if err != nil {
		e.log.Error("Unable to create state", "error", err)
		return err
	}
	defer f.Close()

	ne := json.NewEncoder(f)
	return ne.Encode(e.state)
}

// generateProviders returns providers grouped together in order of execution
func generateProvidersImpl(c *config.Config, cc *Clients, l hclog.Logger) [][]providers.Provider {
	oc := make([][]providers.Provider, 7)
	oc[0] = make([]providers.Provider, 0)
	oc[1] = make([]providers.Provider, 0)
	oc[2] = make([]providers.Provider, 0)
	oc[3] = make([]providers.Provider, 0)
	oc[4] = make([]providers.Provider, 0)
	oc[5] = make([]providers.Provider, 0)
	oc[6] = make([]providers.Provider, 0)

	p := providers.NewNetwork(c.WAN, cc.Docker, l)
	oc[0] = append(oc[0], p)

	for _, n := range c.Networks {
		p := providers.NewNetwork(n, cc.Docker, l)
		oc[0] = append(oc[0], p)
	}

	for _, c := range c.Containers {
		p := providers.NewContainer(*c, cc.ContainerTasks, l)
		oc[1] = append(oc[1], p)
	}

	for _, c := range c.Ingresses {
		p := providers.NewIngress(*c, cc.ContainerTasks, l)
		oc[1] = append(oc[1], p)
	}

	if c.Docs != nil {
		p := providers.NewDocs(c.Docs, cc.ContainerTasks, l)
		oc[1] = append(oc[1], p)
	}

	for _, c := range c.Clusters {
		p := providers.NewCluster(*c, cc.ContainerTasks, cc.Kubernetes, cc.HTTP, l)
		oc[2] = append(oc[2], p)
	}

	for _, c := range c.HelmCharts {
		p := providers.NewHelm(c, cc.Kubernetes, l)
		oc[3] = append(oc[3], p)
	}

	for _, c := range c.K8sConfig {
		p := providers.NewK8sConfig(c, cc.Kubernetes, l)
		oc[4] = append(oc[4], p)
	}

	for _, c := range c.LocalExecs {
		p := providers.NewLocalExec(c, cc.Command, l)
		oc[6] = append(oc[6], p)
	}

	for _, c := range c.RemoteExecs {
		p := providers.NewRemoteExec(*c, cc.ContainerTasks, l)
		oc[6] = append(oc[6], p)
	}

	return oc
}
