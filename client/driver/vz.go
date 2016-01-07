package driver

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/client/config"
	cstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/client/fingerprint"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/mitchellh/mapstructure"
)

var (
	reVzVersion = regexp.MustCompile(`vzctl\D*(\d+.\d+.\d+)`)
)

const (
	// Factor used to convert Mbytes to number of memory pages. For x86 page size is 4Kb
	pagesFactor = 1024 / 4
)

// VzDriver is a driver for running Virtuozzo containers
// We attempt to chose sane defaults for now, with more configuration available
// planned in the future
type VzDriver struct {
	DriverContext
	fingerprint.StaticFingerprinter
}

// VzDriverConfig struct describes driver configuration
type VzDriverConfig struct {
	OSTemplate  string `mapstructure:"os_template"`
	ConfigName  string `mapstructure:"config_name"`  // /etc/vz/conf/ve-NAME.conf-sample
	PrivatePath string `mapstructure:"private_path"` // VE_PRIVATE
	RootPath    string `mapstructure:"root_path"`    // VE_ROOT
}

// vzHandle is returned from Start/Open as a handle to the PID
type vzHandle struct {
	ctID   string
	logger *log.Logger
	waitCh chan *cstructs.WaitResult
	doneCh chan struct{}
}

// NewVzDriver is used to create a new exec driver
func NewVzDriver(ctx *DriverContext) Driver {
	return &VzDriver{DriverContext: *ctx}
}

// Fingerprint is used to detect whether the client can use this driver
func (d *VzDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}

	// Only enable if we are root
	if syscall.Geteuid() != 0 {
		d.logger.Printf("[DEBUG] driver.vz: must run as root user, disabling")
		return false, nil
	}

	outBytes, err := exec.Command("vzctl", "--version").Output()
	if err != nil {
		return false, nil
	}
	out := strings.TrimSpace(string(outBytes))

	matches := reVzVersion.FindStringSubmatch(out)
	if len(matches) != 2 {
		return false, fmt.Errorf("Unable to parse Virtuozzo version string: %#v", matches)
	}

	node.Attributes["driver.vz"] = "1"
	node.Attributes["driver.vz.version"] = matches[1]

	return true, nil
}

// Start will create container from the template, mount and start it.
func (d *VzDriver) Start(ctx *ExecContext, task *structs.Task) (DriverHandle, error) {
	var driverConfig VzDriverConfig
	if err := mapstructure.WeakDecode(task.Config, &driverConfig); err != nil {
		return nil, err
	}

	d.logger.Print("[DEBUG] driver.vz: Start() is invoked")

	// Validate task configuration
	source, ok := task.Config["os_template"]
	if !ok || source == "" {
		return nil, fmt.Errorf("Missing OS template for VZ driver")
	}

	ctID := randomCTID()

	// Build the "create" command.
	createArgs := []string{"create", ctID, "--ostemplate", driverConfig.OSTemplate}

	// Check if the user has overriden "private" and "root".
	if privatePath, ok := task.Config["private_path"]; ok {
		createArgs = append(createArgs, fmt.Sprintf("--private='%v'", privatePath))
	}
	if rootPath, ok := task.Config["root_path"]; ok {
		createArgs = append(createArgs, fmt.Sprintf("--root='%v'", rootPath))
	}
	if configName, ok := task.Config["config_name"]; ok {
		createArgs = append(createArgs, fmt.Sprintf("--config=%v", configName))
	}

	// Create the container
	outBytes, err := exec.Command("vzctl", createArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error creating VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}
	d.logger.Printf("[INFO] driver.vz: Created VZ container: %s", ctID)

	// Build the "set" command.
	setArgs := []string{"set", ctID, "--save"}

	// Configure the container
	if task.Resources.CPU != 0 {
		cpuLimit := fmt.Sprintf("%dm", task.Resources.CPU)
		setArgs = append(setArgs, fmt.Sprintf("--cpulimit=%v", cpuLimit))
	}

	if task.Resources.MemoryMB != 0 {
		memPages := fmt.Sprintf("%d", task.Resources.MemoryMB*pagesFactor)
		setArgs = append(setArgs, fmt.Sprintf("--physpages=%v", memPages))
	}

	// TODO: Implement network configuration

	outBytes, err = exec.Command("vzctl", setArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error configuring VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	// Create and Return Handle
	h := &vzHandle{
		ctID:   ctID,
		logger: d.logger,
		doneCh: make(chan struct{}),
		waitCh: make(chan *cstructs.WaitResult, 1),
	}
	go h.run()

	d.logger.Print("[DEBUG] driver.vz: Start() is done")
	return h, nil
}

// Open tries to reopen a previousely allocated task
func (d *VzDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	d.logger.Print("[DEBUG] driver.vz: Open() is invoked")

	// Return a driver handle
	h := &vzHandle{
		ctID:   handleID,
		logger: d.logger,
		doneCh: make(chan struct{}),
		waitCh: make(chan *cstructs.WaitResult, 1),
	}

	go h.run()

	d.logger.Print("[DEBUG] driver.vz: Open() is done")
	return h, nil
}

// ID returns a handle to the PID
func (h *vzHandle) ID() string {
	return h.ctID
}

// WaitCh returns a handle to whaitCh channel
func (h *vzHandle) WaitCh() chan *cstructs.WaitResult {
	return h.waitCh
}

// Update tries to update a running task. No-op
func (h *vzHandle) Update(task *structs.Task) error {
	h.logger.Print("[DEBUG] driver.vz: Update is invoked")
	// Update is not possible
	// TODO: Implement update!
	return nil
}

// Kill frocely stops and deletes the VM
func (h *vzHandle) Kill() error {
	// Close the "doneСР" channel to prevent container auto-recovery
	h.logger.Print("[DEBUG] driver.vz: Closing channel 'doneCh'")
	close(h.doneCh)

	// Start the VM
	outBytes, err := exec.Command("vzctl", "stop", h.ctID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error stopping VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	// Delete the VM
	outBytes, err = exec.Command("vzctl", "delete", h.ctID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error deleting VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	h.logger.Printf("[INFO] driver.vz: VZ container was stopped and deleted: %s", h.ctID)
	return nil
}

func (h *vzHandle) run() {
	var res cstructs.WaitResult

OUTER:
	for {
		// Skip recovery if "doneCh" is closed
		select {
		case <-h.doneCh:
			h.logger.Print("[DEBUG] driver.vz: Channel 'doneCh' is closed. We are done ")
			res.ExitCode = 0
			break OUTER
		default:
		}

		status, err := CTState(h.ctID)
		if err != nil {
			res.ExitCode = 1
			res.Err = err
			break
		}

		switch status {
		case "stopped", "suspended", "mounted":
			h.logger.Printf("[DEBUG] driver.vz: VZ container is in state '%s'. Starting...", status)
			// Start the container
			if err = startCT(h.ctID); err != nil {
				h.logger.Print(err)
				res.ExitCode = 1
				res.Err = err
				break
			}
			h.logger.Printf("[INFO] driver.vz: Started VZ container: %s", h.ctID)
		}

		// Wait before retry
		time.Sleep(5 * time.Second)
	}

	h.waitCh <- &res
	h.logger.Print("[DEBUG] driver.vz: Closing channel 'waitCh'")
	close(h.waitCh)
}

// CTState returns the state of specified container (running, stopped, etc)
func CTState(ctID string) (string, error) {
	outBytes, err := exec.Command("vzlist", ctID, "-o", "status", "--no-header").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Failed to list VZ container %s: %s\n\nOutput: %s",
			ctID, err, string(outBytes))
	}
	return strings.TrimSpace(string(outBytes)), nil
}

// startCT runs the specified container
func startCT(ctID string) error {
	outBytes, err := exec.Command("vzctl", "start", ctID, "--wait").CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to start VZ container %s: %s\n\nOutput: %s",
			ctID, err, string(outBytes))
	}
	return nil
}

// randomCTID generates a pseudo-random container ID. It ensures that such ID
// is not in use bu any other node (for cluster Virtuozzo installations)
func randomCTID() string {
	for {
		id := rand.Intn(10000000) + 1
		confPath := fmt.Sprintf("/vz/private/%d/ve.conf", id)
		if _, err := os.Stat(confPath); err != nil {
			return strconv.Itoa(id)
		}
	}
}
