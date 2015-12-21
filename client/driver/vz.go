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
	// if task.Resources == nil {
	// 	return nil, fmt.Errorf("Resources are not specified")
	// }
	// if task.Resources.MemoryMB == 0 {
	// 	return nil, fmt.Errorf("Memory limit cannot be zero")
	// }
	// if task.Resources.CPU == 0 {
	// 	return nil, fmt.Errorf("CPU limit cannot be zero")
	// }

	ctID := randomCTID()

	// Build the "create" command.
	createArgs := []string{"create", ctID, "--ostemplate", driverConfig.OSTemplate}

	// Check if the user has overriden "private" and "root".
	if privatePath, ok := task.Config["private_path"]; ok {
		createArgs = append(createArgs, fmt.Sprintf("--private=%v", privatePath))
	}
	if rootPath, ok := task.Config["root_path"]; ok {
		createArgs = append(createArgs, fmt.Sprintf("--root=%v", rootPath))
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

	// Configure the container
	memPages := fmt.Sprintf("%d", task.Resources.MemoryMB*256) // Page size is 4Kb for x86
	cpuLimit := fmt.Sprintf("%dm", task.Resources.CPU)
	outBytes, err = exec.Command("vzctl", "set", ctID,
		"--physpages", memPages,
		"--cpulimit", cpuLimit,
		"--save",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error configuring VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	// TODO: Implement network configuration

	// Start the VM
	outBytes, err = exec.Command("vzctl", "start", ctID, "--wait").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error starting VZ container: %s\n\nOutput: %s",
			err, string(outBytes))
	}
	d.logger.Printf("[INFO] driver.vz: Started VZ container: %s", ctID)

	// It takes some time for container
	// time.Sleep(2 * time.Second)

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

	cmd := exec.Command("vzlist", handleID, "-o", "status", "--no-header")
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Failed to list VZ container %s: %v", handleID, err)
	}

	status := strings.TrimSpace(string(outBytes))
	if status != "running" {
		return nil, fmt.Errorf("VZ container %s is in '%s' state. Expected: 'running'", handleID, status)
	}

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

	for {
		cmd := exec.Command("vzlist", h.ctID, "-o", "status", "--no-header")
		outBytes, err := cmd.Output()
		if err != nil {
			h.logger.Print("[DEBUG] driver.vz: VZ container does not exist!")
			res.ExitCode = 1
			res.Err = err
			break
		}

		status := strings.TrimSpace(string(outBytes))
		if status != "running" {
			h.logger.Printf("[DEBUG] driver.vz: VZ container is not running! State: %s", status)
			res.ExitCode = 0
			break
		}

		// Wait before retry
		time.Sleep(5 * time.Second)
	}

	h.logger.Print("[DEBUG] driver.vz: Closing channel 'doneCh'")
	close(h.doneCh)

	h.waitCh <- &res
	h.logger.Print("[DEBUG] driver.vz: Closing channel 'waitCh'")
	close(h.waitCh)
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
