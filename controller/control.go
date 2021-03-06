package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"

	"github.com/rancher/longhorn-engine/types"
	"github.com/rancher/longhorn-engine/util"
)

const (
	RPCTimeout = 60 * time.Second

	LauncherBinary = "longhorn-engine-launcher"
)

type Controller struct {
	sync.RWMutex
	Name       string
	size       int64
	sectorSize int64
	replicas   []types.Replica
	factory    types.BackendFactory
	backend    *replicator
	frontend   types.Frontend
	launcher   string
	launcherID string

	listenAddr string
	listenPort string
	RestServer *http.Server
	shutdownWG sync.WaitGroup
	lastError  error
}

func NewController(name string, factory types.BackendFactory, frontend types.Frontend, launcher, launcherID string) *Controller {
	c := &Controller{
		factory:    factory,
		Name:       name,
		frontend:   frontend,
		launcher:   launcher,
		launcherID: launcherID,
	}
	c.reset()
	return c
}

func (c *Controller) StartRestServer() error {
	if c.RestServer == nil {
		return fmt.Errorf("cannot find rest server")
	}
	c.shutdownWG.Add(1)
	go func() {
		addr := c.RestServer.Addr
		err := c.RestServer.ListenAndServe()
		logrus.Errorf("Rest server at %v is down: %v", addr, err)
		c.lastError = err
		c.shutdownWG.Done()
	}()
	logrus.Infof("Listening on %s", c.RestServer.Addr)

	return nil
}

func (c *Controller) WaitForShutdown() error {
	c.shutdownWG.Wait()
	return c.lastError
}

func (c *Controller) AddReplica(address string) error {
	return c.addReplica(address, true)
}

func (c *Controller) hasWOReplica() bool {
	for _, i := range c.replicas {
		if i.Mode == types.WO {
			return true
		}
	}
	return false
}

func (c *Controller) canAdd(address string) (bool, error) {
	if c.hasReplica(address) {
		return false, nil
	}
	if c.hasWOReplica() {
		return false, fmt.Errorf("Can only have one WO replica at a time")
	}
	return true, nil
}

func (c *Controller) addReplica(address string, snapshot bool) error {
	c.Lock()
	if ok, err := c.canAdd(address); !ok {
		c.Unlock()
		return err
	}
	c.Unlock()

	newBackend, err := c.factory.Create(address)
	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	return c.addReplicaNoLock(newBackend, address, snapshot)
}

func (c *Controller) Snapshot(name string, labels map[string]string) (string, error) {
	c.Lock()
	defer c.Unlock()

	if name == "" {
		name = util.UUID()
	}

	if remain, err := c.backend.RemainSnapshots(); err != nil {
		return "", err
	} else if remain <= 0 {
		return "", fmt.Errorf("Too many snapshots created")
	}

	created := util.Now()
	return name, c.handleErrorNoLock(c.backend.Snapshot(name, true, created, labels))
}

func (c *Controller) addReplicaNoLock(newBackend types.Backend, address string, snapshot bool) error {
	if ok, err := c.canAdd(address); !ok {
		return err
	}

	if snapshot {
		uuid := util.UUID()
		created := util.Now()

		if remain, err := c.backend.RemainSnapshots(); err != nil {
			return err
		} else if remain <= 0 {
			return fmt.Errorf("Too many snapshots created")
		}

		if err := c.backend.Snapshot(uuid, false, created, nil); err != nil {
			newBackend.Close()
			return err
		}
		if err := newBackend.Snapshot(uuid, false, created, nil); err != nil {
			newBackend.Close()
			return err
		}
	}

	c.replicas = append(c.replicas, types.Replica{
		Address: address,
		Mode:    types.WO,
	})

	c.backend.AddBackend(address, newBackend)

	go c.monitoring(address, newBackend)

	return nil
}

func (c *Controller) hasReplica(address string) bool {
	for _, i := range c.replicas {
		if i.Address == address {
			return true
		}
	}
	return false
}

func (c *Controller) RemoveReplica(address string) error {
	c.Lock()
	defer c.Unlock()

	if !c.hasReplica(address) {
		return nil
	}

	for i, r := range c.replicas {
		if r.Address == address {
			if len(c.replicas) == 1 && c.frontend.State() == types.StateUp {
				return fmt.Errorf("Cannot remove last replica if volume is up")
			}
			c.replicas = append(c.replicas[:i], c.replicas[i+1:]...)
			c.backend.RemoveBackend(r.Address)
		}
	}

	return nil
}

func (c *Controller) ListReplicas() []types.Replica {
	return c.replicas
}

func (c *Controller) SetReplicaMode(address string, mode types.Mode) error {
	switch mode {
	case types.ERR:
		c.Lock()
		defer c.Unlock()
	case types.RW:
		c.RLock()
		defer c.RUnlock()
	default:
		return fmt.Errorf("Can not set to mode %s", mode)
	}

	c.setReplicaModeNoLock(address, mode)
	return nil
}

func (c *Controller) setReplicaModeNoLock(address string, mode types.Mode) {
	for i, r := range c.replicas {
		if r.Address == address {
			if r.Mode != types.ERR {
				logrus.Infof("Set replica %v to mode %v", address, mode)
				r.Mode = mode
				c.replicas[i] = r
				c.backend.SetMode(address, mode)
			} else {
				logrus.Infof("Ignore set replica %v to mode %v due to it's ERR",
					address, mode)
			}
		}
	}
}

func (c *Controller) startFrontend() error {
	if len(c.replicas) > 0 && c.frontend != nil {
		if err := c.frontend.Startup(c.Name, c.size, c.sectorSize, c); err != nil {
			// FATAL
			logrus.Fatalf("Failed to start up frontend: %v", err)
			// This will never be reached
			return err
		}
		if c.launcher != "" {
			if err := c.launcherStartFrontend(); err != nil {
				logrus.Fatalf("Failed to start up frontend: %v", err)
				// This will never be reached
				return err
			}
		}
	}
	return nil
}

func (c *Controller) Start(addresses ...string) error {
	var expectedRevision int64

	c.Lock()
	defer c.Unlock()

	if len(addresses) == 0 {
		return nil
	}

	if len(c.replicas) > 0 {
		return nil
	}

	c.reset()

	defer c.startFrontend()

	first := true
	for _, address := range addresses {
		newBackend, err := c.factory.Create(address)
		if err != nil {
			return err
		}

		newSize, err := newBackend.Size()
		if err != nil {
			return err
		}

		newSectorSize, err := newBackend.SectorSize()
		if err != nil {
			return err
		}

		if first {
			first = false
			c.size = newSize
			c.sectorSize = newSectorSize
		} else if c.size != newSize {
			return fmt.Errorf("Backend sizes do not match %d != %d", c.size, newSize)
		} else if c.sectorSize != newSectorSize {
			return fmt.Errorf("Backend sizes do not match %d != %d", c.sectorSize, newSectorSize)
		}

		if err := c.addReplicaNoLock(newBackend, address, false); err != nil {
			return err
		}
		// We will validate this later
		c.setReplicaModeNoLock(address, types.RW)
	}

	revisionCounters := make(map[string]int64)
	for _, r := range c.replicas {
		counter, err := c.backend.GetRevisionCounter(r.Address)
		if err != nil {
			return err
		}
		if counter > expectedRevision {
			expectedRevision = counter
		}
		revisionCounters[r.Address] = counter
	}

	for address, counter := range revisionCounters {
		if counter != expectedRevision {
			logrus.Errorf("Revision conflict detected! Expect %v, got %v in replica %v. Mark as ERR",
				expectedRevision, counter, address)
			c.setReplicaModeNoLock(address, types.ERR)
		}
	}

	return nil
}

func (c *Controller) WriteAt(b []byte, off int64) (int, error) {
	c.RLock()
	if off < 0 || off+int64(len(b)) > c.size {
		err := fmt.Errorf("EOF: Write of %v bytes at offset %v is beyond volume size %v", len(b), off, c.size)
		c.RUnlock()
		return 0, err
	}
	n, err := c.backend.WriteAt(b, off)
	c.RUnlock()
	if err != nil {
		return n, c.handleError(err)
	}
	return n, err
}

func (c *Controller) ReadAt(b []byte, off int64) (int, error) {
	c.RLock()
	if off < 0 || off+int64(len(b)) > c.size {
		err := fmt.Errorf("EOF: Read of %v bytes at offset %v is beyond volume size %v", len(b), off, c.size)
		c.RUnlock()
		return 0, err
	}
	n, err := c.backend.ReadAt(b, off)
	c.RUnlock()
	if err != nil {
		return n, c.handleError(err)
	}
	return n, err
}

func (c *Controller) handleErrorNoLock(err error) error {
	if bErr, ok := err.(*BackendError); ok {
		if len(bErr.Errors) > 0 {
			for address, replicaErr := range bErr.Errors {
				logrus.Errorf("Setting replica %s to ERR due to: %v", address, replicaErr)
				c.setReplicaModeNoLock(address, types.ERR)
			}
			// if we still have a good replica, do not return error
			for _, r := range c.replicas {
				if r.Mode == types.RW {
					logrus.Errorf("Ignoring error because %s is mode RW: %v", r.Address, err)
					err = nil
					break
				}
			}
		}
	}
	if err != nil {
		logrus.Errorf("I/O error: %v", err)
	}
	return err
}

func (c *Controller) handleError(err error) error {
	c.Lock()
	defer c.Unlock()
	return c.handleErrorNoLock(err)
}

func (c *Controller) reset() {
	c.replicas = []types.Replica{}
	c.backend = &replicator{}
}

func (c *Controller) Close() error {
	return c.Shutdown()
}

func (c *Controller) shutdownFrontend() error {
	// Make sure writing data won't be blocked
	c.RLock()
	defer c.RUnlock()

	// shutdown launcher's frontend if applied
	if c.launcher != "" {
		logrus.Infof("Asking the launcher to shutdown the frontend")
		if err := c.launcherShutdownFrontend(); err != nil {
			return err
		}
	}
	if c.frontend != nil {
		return c.frontend.Shutdown()
	}
	return nil
}

func (c *Controller) shutdownBackend() error {
	c.Lock()
	defer c.Unlock()

	err := c.backend.Close()
	c.reset()

	return err
}

func (c *Controller) Shutdown() error {
	/*
		Need to shutdown frontend first because it will write
		the final piece of data to backend
	*/
	err := c.shutdownFrontend()
	if err != nil {
		logrus.Error("Error when shutting down frontend:", err)
	}
	err = c.shutdownBackend()
	if err != nil {
		logrus.Error("Error when shutting down backend:", err)
	}
	return nil
}

func (c *Controller) Size() (int64, error) {
	return c.size, nil
}

func (c *Controller) monitoring(address string, backend types.Backend) {
	monitorChan := backend.GetMonitorChannel()

	if monitorChan == nil {
		return
	}

	logrus.Infof("Start monitoring %v", address)
	err := <-monitorChan
	if err != nil {
		logrus.Errorf("Backend %v monitoring failed, mark as ERR: %v", address, err)
		c.SetReplicaMode(address, types.ERR)
	}
	logrus.Infof("Monitoring stopped %v", address)
}

func (c *Controller) Endpoint() string {
	return c.frontend.Endpoint()
}

func (c *Controller) Frontend() string {
	return c.frontend.FrontendName()
}

func (c *Controller) UpdatePort(newPort int) error {
	oldServer := c.RestServer
	if oldServer == nil {
		return fmt.Errorf("old rest server doesn't exist")
	}
	oldAddr := c.RestServer.Addr
	handler := c.RestServer.Handler
	addrs := strings.Split(oldAddr, ":")
	newAddr := addrs[0] + ":" + strconv.Itoa(newPort)

	logrus.Infof("About to change to listen to %v", newAddr)
	newServer := &http.Server{
		Addr:    newAddr,
		Handler: handler,
	}
	c.RestServer = newServer
	c.StartRestServer()

	// this will immediate shutdown all the existing connections. the
	// pending http requests would error out
	if err := oldServer.Close(); err != nil {
		logrus.Warnf("Failed to close old server at %v: %v", oldAddr, err)
	}
	return nil
}

func (c *Controller) launcherStartFrontend() error {
	if c.launcher == "" {
		return nil
	}
	args := []string{
		"--url", c.launcher,
		"frontend-start",
		"--id", c.launcherID,
	}
	if _, err := util.Execute(LauncherBinary, args...); err != nil {
		return fmt.Errorf("failed to start frontend: %v", err)
	}
	return nil
}

func (c *Controller) launcherShutdownFrontend() error {
	if c.launcher == "" {
		return nil
	}
	args := []string{
		"--url", c.launcher,
		"frontend-shutdown",
		"--id", c.launcherID,
	}
	if _, err := util.Execute(LauncherBinary, args...); err != nil {
		return fmt.Errorf("failed to shutdown frontend: %v", err)
	}
	return nil
}
