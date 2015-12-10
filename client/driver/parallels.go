package driver

import (
	"archive/tar"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	cstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/client/fingerprint"
	"github.com/hashicorp/nomad/client/getter"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/mitchellh/mapstructure"
)

var (
	reParallelsVersion = regexp.MustCompile(`prlctl version (\d+\.\d+.\d+)`)
)

// ParallelsDriver is a driver for running VMs via Parallels Desktop for Mac
// We attempt to chose sane defaults for now, with more configuration available
// planned in the future
type ParallelsDriver struct {
	DriverContext
	fingerprint.StaticFingerprinter
}

// ParallelsDriverConfig struct describes driver configuration
type ParallelsDriverConfig struct {
	ArtifactSource string `mapstructure:"artifact_source"`
	Checksum       string `mapstructure:"checksum"`
}

// prlHandle is returned from Start/Open as a handle to the PID
type prlHandle struct {
	vmID   string
	logger *log.Logger
	waitCh chan *cstructs.WaitResult
	doneCh chan struct{}
}

// NewParallelsDriver is used to create a new exec driver
func NewParallelsDriver(ctx *DriverContext) Driver {
	return &ParallelsDriver{DriverContext: *ctx}
}

// Fingerprint is used to detect whether the client can use this driver
func (d *ParallelsDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return false, nil
	}

	outBytes, err := exec.Command("prlctl", "--version").Output()
	if err != nil {
		return false, nil
	}
	out := strings.TrimSpace(string(outBytes))

	matches := reParallelsVersion.FindStringSubmatch(out)
	if len(matches) != 2 {
		return false, fmt.Errorf("Unable to parse Parallels Desktop version string: %#v", matches)
	}
	// TODO: Add PDFM version check.

	node.Attributes["driver.parallels"] = "1"
	node.Attributes["driver.parallels.version"] = matches[1]

	return true, nil
}

// Start will pull down an artifact, save it to Task Allocation Dir, unpack
// the PVM image from it, register the image and finally run it.
func (d *ParallelsDriver) Start(ctx *ExecContext, task *structs.Task) (DriverHandle, error) {
	var driverConfig ParallelsDriverConfig
	if err := mapstructure.WeakDecode(task.Config, &driverConfig); err != nil {
		return nil, err
	}

	d.logger.Print("[DEBUG] driver.parallels: Start() is invoked")

	// Validate task configuration
	source, ok := task.Config["artifact_source"]
	if !ok || source == "" {
		return nil, fmt.Errorf("Missing artifact source for Parallels driver")
	}
	if task.Resources == nil {
		return nil, fmt.Errorf("Resources are not specified")
	}
	if task.Resources.MemoryMB == 0 {
		return nil, fmt.Errorf("Memory limit cannot be zero")
	}
	if task.Resources.CPU == 0 {
		return nil, fmt.Errorf("CPU limit cannot be zero")
	}

	// Get the task local directory
	taskDir, ok := ctx.AllocDir.TaskDirs[d.DriverContext.taskName]
	if !ok {
		return nil, fmt.Errorf("Could not find task directory for task: %v", d.DriverContext.taskName)
	}

	// Proceed to download an artifact to be unpacked
	artPath, err := getter.GetArtifact(
		filepath.Join(taskDir, allocdir.TaskLocal),
		driverConfig.ArtifactSource,
		driverConfig.Checksum,
		d.logger,
	)
	if err != nil {
		return nil, err
	}

	d.logger.Printf("[DEBUG] driver.parallels: Artifact path is: %s", artPath)

	// Name should be unique. Otherwise PDFM will rename in case of collision while "prlctl register"
	vmImg := fmt.Sprintf("%s.pvm", ctx.AllocID)
	vmHome := filepath.Join(taskDir, allocdir.TaskLocal, vmImg)

	// Untar the artifact
	if err := untarArtifact(artPath, vmHome); err != nil {
		return nil, fmt.Errorf("Error unpacking the artifact: %v", err)
	}

	d.logger.Printf("[DEBUG] driver.parallels: VM home is: %s", vmHome)

	// Register the VM
	outBytes, err := exec.Command("prlctl", "register", vmHome).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error registering Parallels Desktop VM: %s\n\nOutput: %s",
			err, string(outBytes))
	}
	d.logger.Printf("[INFO] driver.parallels: Registered Parallels Desktop VM image: %s", vmHome)

	vmID, err := parseVMID(vmHome)
	if err != nil {
		return nil, fmt.Errorf("Error fetching Parallels Desktop VM ID: %s", err)
	}

	mem := fmt.Sprintf("%d", task.Resources.MemoryMB)
	cpus := fmt.Sprintf("%d", task.Resources.CPU)
	outBytes, err = exec.Command("prlctl", "set", vmID,
		"--memsize", mem,
		"--cpus", cpus,
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error configuring Parallels Desktop VM: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	// TODO: Implement port forwarding and network configuration

	// Start the VM
	outBytes, err = exec.Command("prlctl", "start", vmID).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error starting Parallels Desktop VM: %s\n\nOutput: %s",
			err, string(outBytes))
	}
	d.logger.Printf("[INFO] driver.parallels: Started Parallels Desktop VM: %s", vmID)

	// It takes some time for Parallels dispatcher to spawn the VM process
	time.Sleep(2 * time.Second)

	// Create and Return Handle
	h := &prlHandle{
		vmID:   vmID,
		logger: d.logger,
		doneCh: make(chan struct{}),
		waitCh: make(chan *cstructs.WaitResult, 1),
	}
	go h.run()

	d.logger.Print("[DEBUG] driver.parallels: Start() is done")
	return h, nil
}

// Open tries to reopen a previousely allocated task
func (d *ParallelsDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	d.logger.Print("[DEBUG] driver.parallels: Open() is invoked")

	cmd := exec.Command("prlctl", "list", handleID, "-o", "status", "--no-header")
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Failed to list Parallels VM %s: %v", handleID, err)
	}

	status := strings.TrimSpace(string(outBytes))
	if status != "running" {
		return nil, fmt.Errorf("Parallels VM %s is in '%s' state. Expected: 'running'", handleID, status)
	}

	// Return a driver handle
	h := &prlHandle{
		vmID:   handleID,
		logger: d.logger,
		doneCh: make(chan struct{}),
		waitCh: make(chan *cstructs.WaitResult, 1),
	}

	go h.run()

	d.logger.Print("[DEBUG] driver.parallels: Open() is done")
	return h, nil
}

// ID returns a handle to the PID
func (h *prlHandle) ID() string {
	return h.vmID
}

// WaitCh returns a handle to whaitCh channel
func (h *prlHandle) WaitCh() chan *cstructs.WaitResult {
	return h.waitCh
}

// Update tries to update a running task. No-op
func (h *prlHandle) Update(task *structs.Task) error {
	h.logger.Print("[DEBUG] driver.parallels: Update is invoked")
	// Update is not possible
	// TODO: Implement update!
	return nil
}

// Kill frocely stops and deletes the VM
func (h *prlHandle) Kill() error {
	// Start the VM
	outBytes, err := exec.Command("prlctl", "stop", h.vmID, "--kill").CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error stopping Parallels Desktop VM: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	// Delete the VM
	outBytes, err = exec.Command("prlctl", "delete", h.vmID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error deleting Parallels Desktop VM: %s\n\nOutput: %s",
			err, string(outBytes))
	}

	h.logger.Printf("[INFO] driver.parallels: Parallels Desktop VM was stopped and deleted: %s", h.vmID)
	return nil
}

func (h *prlHandle) run() {
	var res cstructs.WaitResult

	for {
		cmd := exec.Command("prlctl", "list", h.vmID, "-o", "status", "--no-header")
		outBytes, err := cmd.Output()
		if err != nil {
			h.logger.Print("[DEBUG] driver.parallels: Parallels VM does not exist!")
			res.ExitCode = 1
			res.Err = err
			break
		}

		status := strings.TrimSpace(string(outBytes))
		if status != "running" {
			h.logger.Printf("[DEBUG] driver.parallels: Parallels VM is not running! State: %s", status)
			res.ExitCode = 0
			break
		}

		// Wait before retry
		time.Sleep(5 * time.Second)
	}

	h.logger.Print("[DEBUG] driver.parallels: Closing channel 'doneCh'")
	close(h.doneCh)

	h.waitCh <- &res
	h.logger.Print("[DEBUG] driver.parallels: Closing channel 'waitCh'")
	close(h.waitCh)
}

// parseVMID detects UUID of virtual machine located on the specified path
func parseVMID(vmHome string) (string, error) {
	type prlVMConfig struct {
		XMLName xml.Name `xml:"ParallelsVirtualMachine"`
		VMID    string   `xml:"Identification>VmUuid"`
	}

	if isImage := strings.HasSuffix(vmHome, ".pvm"); !isImage {
		return "", fmt.Errorf("Path %s doesn't look like an image of Parallels Desktop VM", vmHome)
	}

	xmlPath := path.Join(vmHome, "config.pvs")
	xmlBytes, err := ioutil.ReadFile(xmlPath)
	if err != nil {
		return "", fmt.Errorf("Error reading config file: %v", err)
	}

	cfg := new(prlVMConfig)
	err = xml.Unmarshal(xmlBytes, &cfg)
	if err != nil {
		return "", fmt.Errorf("Error parsing config file %s: %v", xmlPath, err)
	}

	// Remove brackets and return VM ID
	return strings.Trim(cfg.VMID, "{}"), nil
}

// untarArtifact finds PVM image inside the artifact "fpath" (Vagrant boxes
// are also supported). It unpacks PVM image's content to the specified imgPath.
// Other files from artifact (if available) will not be unpacked.
func untarArtifact(fpath, imgPath string) error {
	fr, err := os.Open(fpath)
	defer fr.Close()
	if err != nil {
		return err
	}

	gr, err := gzip.NewReader(fr)
	defer gr.Close()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(imgPath, 0755); err != nil {
		return err
	}

	tr := tar.NewReader(gr)
	rePVMDir := regexp.MustCompile(`[^\/]*\.pvm\/(.*)`)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return err
		}

		matches := rePVMDir.FindStringSubmatch(hdr.Name)
		if len(matches) != 2 {
			// Skip files which are not located in *.pvm dir
			continue
		}

		path := filepath.Join(imgPath, matches[1])
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			// Ensure that the parent directory exists
			parentDir := filepath.Dir(path)
			if _, err := os.Lstat(parentDir); err != nil && os.IsNotExist(err) {
				err = os.MkdirAll(parentDir, 0755)
				if err != nil {
					return err
				}
			}
			// Write the file
			ow, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode))
			defer ow.Close()
			if err != nil {
				return err
			}
			if _, err := io.Copy(ow, tr); err != nil {
				return err
			}
		}
	}
	return nil
}
