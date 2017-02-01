package consul

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

func testLogger() *log.Logger {
	if testing.Verbose() {
		return log.New(os.Stderr, "", log.LstdFlags)
	}
	return log.New(ioutil.Discard, "", 0)
}

func testTask() *structs.Task {
	return &structs.Task{
		Name: "taskname",
		Resources: &structs.Resources{
			Networks: []*structs.NetworkResource{
				{
					DynamicPorts: []structs.Port{{Label: "x", Value: 1234}},
				},
			},
		},
		Services: []*structs.Service{
			{
				Name:      "taskname-service",
				PortLabel: "x",
				Tags:      []string{"tag1", "tag2"},
			},
		},
	}
}

// testFakeCtx contains a fake Consul AgentAPI and implements the Exec
// interface to allow testing without running Consul.
type testFakeCtx struct {
	ServiceClient *ServiceClient
	FakeConsul    *fakeConsul
	Task          *structs.Task

	ExecFunc func(ctx context.Context, cmd string, args []string) ([]byte, int, error)
}

// Exec implements the ScriptExecutor interface and will use an alternate
// implementation t.ExecFunc if non-nil.
func (t *testFakeCtx) Exec(ctx context.Context, cmd string, args []string) ([]byte, int, error) {
	if t.ExecFunc == nil {
		// Default impl is just "ok"
		return []byte("ok"), 0, nil
	}
	return t.ExecFunc(ctx, cmd, args)
}

// setupFake creates a testFakeCtx with a ServiceClient backed by a fakeConsul.
// A test Task is also provided.
func setupFake() *testFakeCtx {
	fc := &fakeConsul{
		services:  make(map[string]*api.AgentServiceRegistration),
		checks:    make(map[string]*api.AgentCheckRegistration),
		checkTTLs: make(map[string]int),
	}
	return &testFakeCtx{
		ServiceClient: NewServiceClient(fc, testLogger()),
		FakeConsul:    fc,
		Task:          testTask(),
	}
}

// fakeConsul is a fake in-memory Consul backend for ServiceClient.
type fakeConsul struct {
	// maps of what services and checks have been registered
	services map[string]*api.AgentServiceRegistration
	checks   map[string]*api.AgentCheckRegistration
	mu       sync.Mutex

	// when UpdateTTL is called the check ID will have its counter inc'd
	checkTTLs map[string]int
}

func (c *fakeConsul) CheckRegister(check *api.AgentCheckRegistration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[check.ID] = check
	return nil
}

func (c *fakeConsul) CheckDeregister(checkID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.checks, checkID)
	delete(c.checkTTLs, checkID)
	return nil
}

func (c *fakeConsul) ServiceRegister(service *api.AgentServiceRegistration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.services[service.ID] = service
	return nil
}

func (c *fakeConsul) ServiceDeregister(serviceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.services, serviceID)
	return nil
}

func (c *fakeConsul) UpdateTTL(id string, output string, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	check, ok := c.checks[id]
	if !ok {
		return fmt.Errorf("unknown check id: %q", id)
	}
	// Flip initial status to passing
	check.Status = "passing"
	c.checkTTLs[id]++
	return nil
}

func TestConsul_ChangeTags(t *testing.T) {
	ctx := setupFake()

	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, nil); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}

	// Manually call sync() since Run() isn't running
	if err := ctx.ServiceClient.sync(); err != nil {
		t.Fatalf("unexpected error syncing task: %v", err)
	}

	if n := len(ctx.FakeConsul.services); n != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", n, ctx.FakeConsul.services)
	}

	origKey := ""
	for k, v := range ctx.FakeConsul.services {
		origKey = k
		if v.Name != ctx.Task.Services[0].Name {
			t.Errorf("expected Name=%q != %q", ctx.Task.Services[0].Name, v.Name)
		}
		if !reflect.DeepEqual(v.Tags, ctx.Task.Services[0].Tags) {
			t.Errorf("expected Tags=%v != %v", ctx.Task.Services[0].Tags, v.Tags)
		}
	}

	// Changing a tag removes old entry before adding new one
	ctx.ServiceClient.RemoveTask("allocid", ctx.Task)
	ctx.Task.Services[0].Tags[0] = "newtag"
	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, nil); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}
	if err := ctx.ServiceClient.sync(); err != nil {
		t.Fatalf("unexpected error syncing task: %v", err)
	}

	if n := len(ctx.FakeConsul.services); n != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", n, ctx.FakeConsul.services)
	}

	for k, v := range ctx.FakeConsul.services {
		if k == origKey {
			t.Errorf("expected key to change but found %q", k)
		}
		if v.Name != ctx.Task.Services[0].Name {
			t.Errorf("expected Name=%q != %q", ctx.Task.Services[0].Name, v.Name)
		}
		if !reflect.DeepEqual(v.Tags, ctx.Task.Services[0].Tags) {
			t.Errorf("expected Tags=%v != %v", ctx.Task.Services[0].Tags, v.Tags)
		}
	}
}

// TestConsul_RegServices tests basic service registration.
func TestConsul_RegServices(t *testing.T) {
	ctx := setupFake()
	port := ctx.Task.Resources.Networks[0].DynamicPorts[0].Value

	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, nil); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}

	// Manually call sync() since Run() isn't running
	if err := ctx.ServiceClient.sync(); err != nil {
		t.Fatalf("unexpected error syncing task: %v", err)
	}

	if n := len(ctx.FakeConsul.services); n != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", n, ctx.FakeConsul.services)
	}
	for _, v := range ctx.FakeConsul.services {
		if v.Name != ctx.Task.Services[0].Name {
			t.Errorf("expected Name=%q != %q", ctx.Task.Services[0].Name, v.Name)
		}
		if !reflect.DeepEqual(v.Tags, ctx.Task.Services[0].Tags) {
			t.Errorf("expected Tags=%v != %v", ctx.Task.Services[0].Tags, v.Tags)
		}
		if v.Port != port {
			t.Errorf("expected Port=%d != %d", port, v.Port)
		}
	}

	// Make a change which will register a new service
	ctx.Task.Services[0].Name = "taskname-service2"
	ctx.Task.Services[0].Tags[0] = "tag3"
	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, nil); err != nil {
		t.Fatalf("unpexpected error registering task: %v", err)
	}

	// Make sure changes don't take affect until sync() is called (since
	// Run() isn't running)
	if n := len(ctx.FakeConsul.services); n != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", n, ctx.FakeConsul.services)
	}
	for _, v := range ctx.FakeConsul.services {
		if reflect.DeepEqual(v.Tags, ctx.Task.Services[0].Tags) {
			t.Errorf("expected Tags to differ, changes applied before sync()")
		}
	}

	// Now sync() and re-check for the applied updates
	if err := ctx.ServiceClient.sync(); err != nil {
		t.Fatalf("unexpected error syncing task: %v", err)
	}
	if n := len(ctx.FakeConsul.services); n != 2 {
		t.Fatalf("expected 2 services but found %d:\n%#v", n, ctx.FakeConsul.services)
	}
	found := false
	for _, v := range ctx.FakeConsul.services {
		if v.Name == ctx.Task.Services[0].Name {
			if found {
				t.Fatalf("found new service name %q twice", v.Name)
			}
			found = true
			if !reflect.DeepEqual(v.Tags, ctx.Task.Services[0].Tags) {
				t.Errorf("expected Tags=%v != %v", ctx.Task.Services[0].Tags, v.Tags)
			}
		}
	}
	if !found {
		t.Fatalf("did not find new service %q", ctx.Task.Services[0].Name)
	}

	// Remove the new task
	ctx.ServiceClient.RemoveTask("allocid", ctx.Task)
	if err := ctx.ServiceClient.sync(); err != nil {
		t.Fatalf("unexpected error syncing task: %v", err)
	}
	if n := len(ctx.FakeConsul.services); n != 1 {
		t.Fatalf("expected 1 service but found %d:\n%#v", n, ctx.FakeConsul.services)
	}
	for _, v := range ctx.FakeConsul.services {
		if v.Name != "taskname-service" {
			t.Errorf("expected original task to survive not %q", v.Name)
		}
	}
}

// TestConsul_ShutdownOK tests the ok path for the shutdown logic in
// ServiceClient.
func TestConsul_ShutdownOK(t *testing.T) {
	ctx := setupFake()

	// Add a script check to make sure its TTL gets updated
	ctx.Task.Services[0].Checks = []*structs.ServiceCheck{
		{
			Name:    "scriptcheck",
			Type:    "script",
			Command: "true",
			// Make check block until shutdown
			Interval:      9000 * time.Hour,
			Timeout:       10 * time.Second,
			InitialStatus: "warning",
		},
	}

	hasShutdown := make(chan struct{})
	go func() {
		ctx.ServiceClient.Run()
		close(hasShutdown)
	}()

	// Register a task and agent
	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, ctx); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}

	agentServices := []*structs.Service{
		{
			Name:      "http",
			Tags:      []string{"nomad"},
			PortLabel: "localhost:2345",
		},
	}
	if err := ctx.ServiceClient.RegisterAgent("client", agentServices); err != nil {
		t.Fatalf("unexpected error registering agent: %v", err)
	}

	// Shutdown should block until all enqueued operations finish.
	if err := ctx.ServiceClient.Shutdown(); err != nil {
		t.Errorf("unexpected error shutting down client: %v", err)
	}

	// assert Run() exits in a timely fashion after Shutdown() exits
	select {
	case <-hasShutdown:
		// ok! Run() exited as expected
	case <-time.After(10 * time.Second):
		t.Fatalf("expected Run() to exit, but it did not")
	}

	// Nothing should be enqueued anymore
	enqueued := (len(ctx.ServiceClient.regServices) + len(ctx.ServiceClient.deregServices) +
		len(ctx.ServiceClient.regChecks) + len(ctx.ServiceClient.deregChecks))
	if enqueued > 0 {
		t.Errorf("%d operations still enqueued", enqueued)
	}

	// UpdateTTL should have been called once for the script check
	if n := len(ctx.FakeConsul.checkTTLs); n != 1 {
		t.Fatalf("expected 1 checkTTL entry but found: %d", n)
	}
	for _, v := range ctx.FakeConsul.checkTTLs {
		if v != 1 {
			t.Fatalf("expected script check to be updated once but found %d", v)
		}
	}
	for _, v := range ctx.FakeConsul.checks {
		if v.Status != "passing" {
			t.Fatalf("expected check to be passing but found %q", v.Status)
		}
	}
}

// TestConsul_ShutdownSlow tests the slow but ok path for the shutdown logic in
// ServiceClient.
func TestConsul_ShutdownSlow(t *testing.T) {
	t.Parallel() // run the slow tests in parallel
	ctx := setupFake()

	// Add a script check to make sure its TTL gets updated
	ctx.Task.Services[0].Checks = []*structs.ServiceCheck{
		{
			Name:    "scriptcheck",
			Type:    "script",
			Command: "true",
			// Make check block until shutdown
			Interval:      9000 * time.Hour,
			Timeout:       5 * time.Second,
			InitialStatus: "warning",
		},
	}

	// Make Exec slow, but not too slow
	ctx.ExecFunc = func(ctx context.Context, cmd string, args []string) ([]byte, int, error) {
		time.Sleep(time.Second)
		return []byte{}, 0, nil
	}

	// Make shutdown wait time just a bit longer than ctx.Exec takes
	ctx.ServiceClient.shutdownWait = 3 * time.Second

	hasShutdown := make(chan struct{})
	go func() {
		ctx.ServiceClient.Run()
		close(hasShutdown)
	}()

	// Register a task and agent
	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, ctx); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}

	// Shutdown should block until all enqueued operations finish.
	preShutdown := time.Now()
	if err := ctx.ServiceClient.Shutdown(); err != nil {
		t.Errorf("unexpected error shutting down client: %v", err)
	}

	// assert Run() exits in a timely fashion after Shutdown() exits
	select {
	case <-hasShutdown:
		// ok! Run() exited as expected
	case <-time.After(10 * time.Second):
		t.Fatalf("expected Run() to exit, but it did not")
	}

	// Shutdown time should have taken: 1s <= shutdown <= 3s
	shutdownTime := time.Now().Sub(preShutdown)
	if shutdownTime < time.Second || shutdownTime > ctx.ServiceClient.shutdownWait {
		t.Errorf("expected shutdown to take >1s and <%s but took: %s", ctx.ServiceClient.shutdownWait, shutdownTime)
	}

	// UpdateTTL should have been called once for the script check
	if n := len(ctx.FakeConsul.checkTTLs); n != 1 {
		t.Fatalf("expected 1 checkTTL entry but found: %d", n)
	}
	for _, v := range ctx.FakeConsul.checkTTLs {
		if v != 1 {
			t.Fatalf("expected script check to be updated once but found %d", v)
		}
	}
	for _, v := range ctx.FakeConsul.checks {
		if v.Status != "passing" {
			t.Fatalf("expected check to be passing but found %q", v.Status)
		}
	}
}

// TestConsul_ShutdownBlocked tests the blocked past deadline path for the
// shutdown logic in ServiceClient.
func TestConsul_ShutdownBlocked(t *testing.T) {
	t.Parallel() // run the slow tests in parallel
	ctx := setupFake()

	// Add a script check to make sure its TTL gets updated
	ctx.Task.Services[0].Checks = []*structs.ServiceCheck{
		{
			Name:    "scriptcheck",
			Type:    "script",
			Command: "true",
			// Make check block until shutdown
			Interval:      9000 * time.Hour,
			Timeout:       9000 * time.Hour,
			InitialStatus: "warning",
		},
	}

	block := make(chan struct{})
	defer close(block) // cleanup after test

	// Make Exec slow, but not too slow
	ctx.ExecFunc = func(ctx context.Context, cmd string, args []string) ([]byte, int, error) {
		<-block
		return []byte{}, 0, nil
	}

	// Use a short shutdown deadline since we're intentionally blocking forever
	ctx.ServiceClient.shutdownWait = time.Second

	hasShutdown := make(chan struct{})
	go func() {
		ctx.ServiceClient.Run()
		close(hasShutdown)
	}()

	// Register a task and agent
	if err := ctx.ServiceClient.RegisterTask("allocid", ctx.Task, ctx); err != nil {
		t.Fatalf("unexpected error registering task: %v", err)
	}

	// Shutdown should block until all enqueued operations finish.
	preShutdown := time.Now()
	err := ctx.ServiceClient.Shutdown()
	if err == nil {
		t.Errorf("expected a timed out error from shutdown")
	}

	// assert Run() exits in a timely fashion after Shutdown() exits
	maxWait := 10 * time.Second
	select {
	case <-hasShutdown:
		// ok! Run() exited as expected
	case <-time.After(maxWait):
		t.Fatalf("expected Run() to exit, but it did not")
	}

	// Shutdown time should have taken 1s; to avoid timing related errors
	// simply test for 1s <= shutdown <= 10s
	shutdownTime := time.Now().Sub(preShutdown)
	if shutdownTime < ctx.ServiceClient.shutdownWait || shutdownTime > maxWait {
		t.Errorf("expected shutdown to take >%s and <%s but took: %s", ctx.ServiceClient.shutdownWait, maxWait, shutdownTime)
	}

	// UpdateTTL should not have been called for the script check
	if n := len(ctx.FakeConsul.checkTTLs); n != 0 {
		t.Fatalf("expected 0 checkTTL entry but found: %d", n)
	}
	for _, v := range ctx.FakeConsul.checks {
		if expected := "warning"; v.Status != expected {
			t.Fatalf("expected check to be %q but found %q", expected, v.Status)
		}
	}
}