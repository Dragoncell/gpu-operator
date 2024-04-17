/*
 * Copyright (c) 2021, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvmdev"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	devchar "github.com/NVIDIA/nvidia-container-toolkit/cmd/nvidia-ctk/system/create-dev-char-symlinks"
	log "github.com/sirupsen/logrus"
	cli "github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/gpu-operator/internal/info"
)

// Component of GPU operator
type Component interface {
	validate() error
	createStatusFile() error
	deleteStatusFile() error
}

// Driver component
type Driver struct{}

// NvidiaFs GDS Driver component
type NvidiaFs struct{}

// CUDA represents spec to run cuda workload
type CUDA struct {
	ctx        context.Context
	kubeClient kubernetes.Interface
}

// Plugin component
type Plugin struct {
	ctx        context.Context
	kubeClient kubernetes.Interface
}

// Toolkit component
type Toolkit struct{}

// MOFED represents spec to validate MOFED driver installation
type MOFED struct {
	ctx        context.Context
	kubeClient kubernetes.Interface
}

// Metrics represents spec to run metrics exporter
type Metrics struct {
	ctx context.Context
}

// VfioPCI represents spec to validate vfio-pci driver
type VfioPCI struct {
	ctx context.Context
}

// VGPUManager represents spec to validate vGPU Manager installation
type VGPUManager struct {
	ctx context.Context
}

// VGPUDevices represents spec to validate vGPU device creation
type VGPUDevices struct {
	ctx context.Context
}

// CCManager represents spec to validate CC Manager installation
type CCManager struct {
	ctx        context.Context
	kubeClient kubernetes.Interface
}

var (
	kubeconfigFlag                string
	nodeNameFlag                  string
	namespaceFlag                 string
	withWaitFlag                  bool
	withWorkloadFlag              bool
	componentFlag                 string
	cleanupAllFlag                bool
	outputDirFlag                 string
	sleepIntervalSecondsFlag      int
	migStrategyFlag               string
	metricsPort                   int
	defaultGPUWorkloadConfigFlag  string
	disableDevCharSymlinkCreation bool
)

// defaultGPUWorkloadConfig is "vm-passthrough" unless
// overridden by defaultGPUWorkloadConfigFlag
var defaultGPUWorkloadConfig = gpuWorkloadConfigVMPassthrough

const (
	// defaultStatusPath indicates directory to create all validation status files
	defaultStatusPath = "/run/nvidia/validations"
	// defaultSleepIntervalSeconds indicates sleep interval in seconds between validation command retries
	defaultSleepIntervalSeconds = 5
	// defaultMetricsPort indicates the port on which the metrics will be exposed.
	defaultMetricsPort = 0
	// hostDevCharPath indicates the path in the container where the host '/dev/char' directory is mounted to
	hostDevCharPath = "/host-dev-char"
	// driverContainerRoot indicates the path on the host where driver container mounts it's root filesystem
	driverContainerRoot = "/run/nvidia/driver"
	// driverHostRoot indicates the path on the host where driver located in can not chroot cases
	driverHostRoot = "/home/kubernetes/bin/nvidia"
	// driverStatusFile indicates status file for containerizeddriver readiness
	driverStatusFile = "driver-ready"
	// hostDriverStatusFile indicates status file for host driver readiness
	hostDriverStatusFile = "host-driver-ready"
	// nvidiaFsStatusFile indicates status file for nvidia-fs driver readiness
	nvidiaFsStatusFile = "nvidia-fs-ready"
	// toolkitStatusFile indicates status file for toolkit readiness
	toolkitStatusFile = "toolkit-ready"
	// pluginStatusFile indicates status file for plugin readiness
	pluginStatusFile = "plugin-ready"
	// cudaStatusFile indicates status file for cuda readiness
	cudaStatusFile = "cuda-ready"
	// mofedStatusFile indicates status file for mofed driver readiness
	mofedStatusFile = "mofed-ready"
	// vfioPCIStatusFile indicates status file for vfio-pci driver readiness
	vfioPCIStatusFile = "vfio-pci-ready"
	// vGPUManagerStatusFile indicates status file for vGPU Manager driver readiness
	vGPUManagerStatusFile = "vgpu-manager-ready"
	// hostVGPUManagerStatusFile indicates status file for host vGPU Manager driver readiness
	hostVGPUManagerStatusFile = "host-vgpu-manager-ready"
	// vGPUDevicesStatusFile is name of the file which indicates vGPU Manager is installed and vGPU devices have been created
	vGPUDevicesStatusFile = "vgpu-devices-ready"
	// ccManagerStatusFile indicates status file for cc-manager readiness
	ccManagerStatusFile = "cc-manager-ready"
	// workloadTypeStatusFile is the name of the file which specifies the workload type configured for the node
	workloadTypeStatusFile = "workload-type"
	// podCreationWaitRetries indicates total retries to wait for plugin validation pod creation
	podCreationWaitRetries = 60
	// podCreationSleepIntervalSeconds indicates sleep interval in seconds between checking for plugin validation pod readiness
	podCreationSleepIntervalSeconds = 5
	// gpuResourceDiscoveryWaitRetries indicates total retries to wait for node to discovery GPU resources
	gpuResourceDiscoveryWaitRetries = 30
	// gpuResourceDiscoveryIntervalSeconds indicates sleep interval in seconds between checking for available GPU resources
	gpuResourceDiscoveryIntervalSeconds = 5
	// genericGPUResourceType indicates the generic name of the GPU exposed by NVIDIA DevicePlugin
	genericGPUResourceType = "nvidia.com/gpu"
	// migGPUResourcePrefix indicates the prefix of the MIG resources exposed by NVIDIA DevicePlugin
	migGPUResourcePrefix = "nvidia.com/mig-"
	// migStrategySingle indicates mixed MIG strategy
	migStrategySingle = "single"
	// pluginWorkloadPodSpecPath indicates path to plugin validation pod definition
	pluginWorkloadPodSpecPath = "/var/nvidia/manifests/plugin-workload-validation.yaml"
	// cudaWorkloadPodSpecPath indicates path to cuda validation pod definition
	cudaWorkloadPodSpecPath = "/var/nvidia/manifests/cuda-workload-validation.yaml"
	// validatorImageEnvName indicates env name for validator image passed
	validatorImageEnvName = "VALIDATOR_IMAGE"
	// validatorImagePullPolicyEnvName indicates env name for validator image pull policy passed
	validatorImagePullPolicyEnvName = "VALIDATOR_IMAGE_PULL_POLICY"
	// validatorImagePullSecretsEnvName indicates env name for validator image pull secrets passed
	validatorImagePullSecretsEnvName = "VALIDATOR_IMAGE_PULL_SECRETS"
	// validatorRuntimeClassEnvName indicates env name for validator runtimeclass passed
	validatorRuntimeClassEnvName = "VALIDATOR_RUNTIME_CLASS"
	// cudaValidatorLabelValue represents label for cuda workload validation pod
	cudaValidatorLabelValue = "nvidia-cuda-validator"
	// pluginValidatorLabelValue represents label for device-plugin workload validation pod
	pluginValidatorLabelValue = "nvidia-device-plugin-validator"
	// MellanoxDeviceLabelKey represents NFD label name for Mellanox devices
	MellanoxDeviceLabelKey = "feature.node.kubernetes.io/pci-15b3.present"
	// GPUDirectRDMAEnabledEnvName represents env name to indicate if GPUDirect RDMA is enabled through GPU Operator
	GPUDirectRDMAEnabledEnvName = "GPU_DIRECT_RDMA_ENABLED"
	// UseHostMOFEDEnvname represents env name to indicate if MOFED is pre-installed on host
	UseHostMOFEDEnvname = "USE_HOST_MOFED"
	// TODO: create a common package to share these variables between operator and validator
	gpuWorkloadConfigLabelKey      = "nvidia.com/gpu.workload.config"
	gpuWorkloadConfigContainer     = "container"
	gpuWorkloadConfigVMPassthrough = "vm-passthrough"
	gpuWorkloadConfigVMVgpu        = "vm-vgpu"
	// CCCapableLabelKey represents NFD label name to indicate if the node is capable to run CC workloads
	CCCapableLabelKey = "nvidia.com/cc.capable"
)

func main() {
	c := cli.NewApp()
	c.Before = validateFlags
	c.Action = start
	c.Version = info.GetVersionString()

	c.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			Value:       "",
			Usage:       "absolute path to the kubeconfig file",
			Destination: &kubeconfigFlag,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name:        "node-name",
			Aliases:     []string{"n"},
			Value:       "",
			Usage:       "the name of the node to deploy plugin validation pod",
			Destination: &nodeNameFlag,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.StringFlag{
			Name:        "namespace",
			Aliases:     []string{"ns"},
			Value:       "",
			Usage:       "the namespace in which the operator resources are deployed",
			Destination: &namespaceFlag,
			EnvVars:     []string{"OPERATOR_NAMESPACE"},
		},
		&cli.BoolFlag{
			Name:        "with-wait",
			Aliases:     []string{"w"},
			Value:       false,
			Usage:       "indicates to wait for validation to complete successfully",
			Destination: &withWaitFlag,
			EnvVars:     []string{"WITH_WAIT"},
		},
		&cli.BoolFlag{
			Name:        "with-workload",
			Aliases:     []string{"l"},
			Value:       true,
			Usage:       "indicates to validate with GPU workload",
			Destination: &withWorkloadFlag,
			EnvVars:     []string{"WITH_WORKLOAD"},
		},
		&cli.StringFlag{
			Name:        "component",
			Aliases:     []string{"c"},
			Value:       "",
			Usage:       "the name of the operator component to validate",
			Destination: &componentFlag,
			EnvVars:     []string{"COMPONENT"},
		},
		&cli.BoolFlag{
			Name:        "cleanup-all",
			Aliases:     []string{"r"},
			Value:       false,
			Usage:       "indicates to cleanup all previous validation status files",
			Destination: &cleanupAllFlag,
			EnvVars:     []string{"CLEANUP_ALL"},
		},
		&cli.StringFlag{
			Name:        "output-dir",
			Aliases:     []string{"o"},
			Value:       defaultStatusPath,
			Usage:       "output directory where all validation status files are created",
			Destination: &outputDirFlag,
			EnvVars:     []string{"OUTPUT_DIR"},
		},
		&cli.IntFlag{
			Name:        "sleep-interval-seconds",
			Aliases:     []string{"s"},
			Value:       defaultSleepIntervalSeconds,
			Usage:       "sleep interval in seconds between command retries",
			Destination: &sleepIntervalSecondsFlag,
			EnvVars:     []string{"SLEEP_INTERVAL_SECONDS"},
		},
		&cli.StringFlag{
			Name:        "mig-strategy",
			Aliases:     []string{"m"},
			Value:       migStrategySingle,
			Usage:       "MIG Strategy",
			Destination: &migStrategyFlag,
			EnvVars:     []string{"MIG_STRATEGY"},
		},
		&cli.IntFlag{
			Name:        "metrics-port",
			Aliases:     []string{"p"},
			Value:       defaultMetricsPort,
			Usage:       "port on which the metrics will be exposed. 0 means disabled.",
			Destination: &metricsPort,
			EnvVars:     []string{"METRICS_PORT"},
		},
		&cli.StringFlag{
			Name:        "default-gpu-workload-config",
			Aliases:     []string{"g"},
			Value:       "",
			Usage:       "default GPU workload config. determines what components to validate by default when sandbox workloads are enabled in the cluster.",
			Destination: &defaultGPUWorkloadConfigFlag,
			EnvVars:     []string{"DEFAULT_GPU_WORKLOAD_CONFIG"},
		},
		&cli.BoolFlag{
			Name:        "disable-dev-char-symlink-creation",
			Value:       false,
			Usage:       "disable creation of symlinks under /dev/char corresponding to NVIDIA character devices",
			Destination: &disableDevCharSymlinkCreation,
			EnvVars:     []string{"DISABLE_DEV_CHAR_SYMLINK_CREATION"},
		},
	}

	// Log version info
	log.Infof("version: %s", c.Version)

	// Handle signals
	go handleSignal()

	// invoke command
	err := c.Run(os.Args)
	if err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("Error: %v", err)
		os.Exit(1)
	}
}

func handleSignal() {
	// Handle signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt,
		syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)

	s := <-stop
	log.Fatalf("Exiting due to signal [%v] notification for pid [%d]", s.String(), os.Getpid())
}

func validateFlags(c *cli.Context) error {
	if componentFlag == "" {
		return fmt.Errorf("invalid -c <component-name> flag: must not be empty string")
	}
	if !isValidComponent() {
		return fmt.Errorf("invalid -c <component-name> flag value: %s", componentFlag)
	}
	if componentFlag == "plugin" {
		if nodeNameFlag == "" {
			return fmt.Errorf("invalid -n <node-name> flag: must not be empty string for plugin validation")
		}
		if namespaceFlag == "" {
			return fmt.Errorf("invalid -ns <namespace> flag: must not be empty string for plugin validation")
		}
	}
	if componentFlag == "cuda" && namespaceFlag == "" {
		return fmt.Errorf("invalid -ns <namespace> flag: must not be empty string for cuda validation")
	}
	if componentFlag == "metrics" {
		if metricsPort == defaultMetricsPort {
			return fmt.Errorf("invalid -p <port> flag: must not be empty or 0 for the metrics component")
		}
		if nodeNameFlag == "" {
			return fmt.Errorf("invalid -n <node-name> flag: must not be empty string for metrics exporter")
		}
	}
	if nodeNameFlag == "" && (componentFlag == "vfio-pci" || componentFlag == "vgpu-manager" || componentFlag == "vgpu-devices") {
		return fmt.Errorf("invalid -n <node-name> flag: must not be empty string for %s validation", componentFlag)
	}

	return nil
}

func isValidComponent() bool {
	switch componentFlag {
	case "driver":
		fallthrough
	case "toolkit":
		fallthrough
	case "cuda":
		fallthrough
	case "metrics":
		fallthrough
	case "plugin":
		fallthrough
	case "mofed":
		fallthrough
	case "vfio-pci":
		fallthrough
	case "vgpu-manager":
		fallthrough
	case "vgpu-devices":
		fallthrough
	case "cc-manager":
		fallthrough
	case "nvidia-fs":
		return true
	default:
		return false
	}
}

func isValidWorkloadConfig(config string) bool {
	return config == gpuWorkloadConfigContainer ||
		config == gpuWorkloadConfigVMPassthrough ||
		config == gpuWorkloadConfigVMVgpu
}

func getWorkloadConfig(ctx context.Context) (string, error) {
	// check if default workload is overridden by flag
	if isValidWorkloadConfig(defaultGPUWorkloadConfigFlag) {
		defaultGPUWorkloadConfig = defaultGPUWorkloadConfigFlag
	}

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("Error getting cluster config - %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return "", fmt.Errorf("Error getting k8s client - %s", err.Error())
	}

	node, err := getNode(ctx, kubeClient)
	if err != nil {
		return "", fmt.Errorf("Error getting node labels - %s", err.Error())
	}

	labels := node.GetLabels()
	value, ok := labels[gpuWorkloadConfigLabelKey]
	if !ok {
		log.Infof("No %s label found; using default workload config: %s", gpuWorkloadConfigLabelKey, defaultGPUWorkloadConfig)
		return defaultGPUWorkloadConfig, nil
	}
	if !isValidWorkloadConfig(value) {
		log.Warnf("%s is an invalid workload config; using default workload config: %s", value, defaultGPUWorkloadConfig)
		return defaultGPUWorkloadConfig, nil
	}
	return value, nil
}

func start(c *cli.Context) error {
	// if cleanup is requested, delete all existing status files(default)
	if cleanupAllFlag {
		// cleanup output directory and create again each time
		err := os.RemoveAll(outputDirFlag)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		}
	}

	// create status directory
	err := os.Mkdir(outputDirFlag, 0755)
	if err != nil && !os.IsExist(err) {
		return err
	}

	switch componentFlag {
	case "driver":
		driver := &Driver{}
		err := driver.validate()
		if err != nil {
			return fmt.Errorf("error validating driver installation: %s", err)
		}
		return nil
	case "nvidia-fs":
		nvidiaFs := &NvidiaFs{}
		err := nvidiaFs.validate()
		if err != nil {
			return fmt.Errorf("error validating nvidia-fs driver installation: %s", err)
		}
		return nil
	case "toolkit":
		toolkit := &Toolkit{}
		err := toolkit.validate()
		if err != nil {
			return fmt.Errorf("error validating toolkit installation: %s", err)
		}
		return nil
	case "cuda":
		cuda := &CUDA{
			ctx: c.Context,
		}
		err := cuda.validate()
		if err != nil {
			return fmt.Errorf("error validating cuda workload: %s", err)
		}
		return nil
	case "plugin":
		plugin := &Plugin{
			ctx: c.Context,
		}
		err := plugin.validate()
		if err != nil {
			return fmt.Errorf("error validating plugin installation: %s", err)
		}
		return nil
	case "mofed":
		mofed := &MOFED{
			ctx: c.Context,
		}
		err := mofed.validate()
		if err != nil {
			return fmt.Errorf("error validating MOFED driver installation: %s", err)
		}
		return nil
	case "metrics":
		metrics := &Metrics{
			ctx: c.Context,
		}
		err := metrics.run()
		if err != nil {
			return fmt.Errorf("error running validation-metrics exporter: %s", err)
		}
		return nil
	case "vfio-pci":
		vfioPCI := &VfioPCI{
			ctx: c.Context,
		}
		err := vfioPCI.validate()
		if err != nil {
			return fmt.Errorf("error validating vfio-pci driver installation: %s", err)
		}
		return nil
	case "vgpu-manager":
		vGPUManager := &VGPUManager{
			ctx: c.Context,
		}
		err := vGPUManager.validate()
		if err != nil {
			return fmt.Errorf("error validating vGPU Manager installation: %s", err)
		}
		return nil
	case "vgpu-devices":
		vGPUDevices := &VGPUDevices{
			ctx: c.Context,
		}
		err := vGPUDevices.validate()
		if err != nil {
			return fmt.Errorf("error validating vGPU devices: %s", err)
		}
		return nil
	case "cc-manager":
		CCManager := &CCManager{
			ctx: c.Context,
		}
		err := CCManager.validate()
		if err != nil {
			return fmt.Errorf("error validating CC Manager installation: %s", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid component specified for validation: %s", componentFlag)
	}
}

func runCommand(command string, args []string, silent bool) error {
	cmd := exec.Command(command, args...)
	if !silent {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

func runCommandWithWait(command string, args []string, sleepSeconds int, silent bool) error {
	for {
		cmd := exec.Command(command, args...)
		if !silent {
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
		}
		fmt.Printf("running command %s with args %v\n", command, args)
		err := cmd.Run()
		if err != nil {
			fmt.Printf("command failed, retrying after %d seconds\n", sleepSeconds)
			time.Sleep(time.Duration(sleepSeconds) * time.Second)
			continue
		}
		return nil
	}
}

func checkChrootForDriverRoot() bool {
	// dev(contains sh, bash) is an important dir for chroot
	essentialDirs := []string{"dev"}
	for _, subdir := range essentialDirs {
		fullpath := fmt.Sprintf("%s/%s", driverContainerRoot, subdir)
		if _, err := os.Stat(fullpath); os.IsNotExist(err) {
			log.Infof("Detected driver root on the host missing %v, which means can not chroot", subdir)
			return false
		}
	}
	return true
}

type DriverRoot struct {
	// a chroot container dir either /host or /run/nvidia/driver
	driverChrootRoot string
	// check the driver installed on the host root or not
	hostRoot bool
	// the command to run nvidia-smi check
	SMIcommand string
	// a driver contaner root where driver installed
	driverContainerRoot string
	// whether driver devices nodes needs to install or not
	deviceNodes bool
}

func getDriverRoot() (driverRoot DriverRoot) {

	// A few possible cases (= means container path maps to host path)
	// a) /host = /                   /run/nvidia/driver = /run/nvidia/driver
	// b) /host = /                   /run/nvidia/driver = /home/kubernetes/bin/nvidia
	// c) /host = /                   /host/usr/bin/nvidia-smi
	// d) /host = /custom-driver      /host/usr/bin/nvidia-smi

	// case c) and case d)
	// check if driver is pre-installed on the host and use host path for validation
	if fileInfo, err := os.Lstat("/host/usr/bin/nvidia-smi"); err == nil && fileInfo.Size() != 0 {
		log.Infof("Detected pre-installed driver on the host")
		return DriverRoot{"/host", true, "nvidia-smi", "/host", true}
	}

	// case b)
	// check if driver root can be chroot, if not, driver is pre-installed on host in a custom driver path
	if !checkChrootForDriverRoot() {
		log.Infof("Detected pre-installed driver on the host on %v driver path", driverHostRoot)
		return DriverRoot{"/host", true, driverHostRoot + "/bin/nvidia-smi", driverContainerRoot, false}
	}

	// case a) driver root can chroot
	return DriverRoot{driverContainerRoot, false, "nvidia-smi", driverContainerRoot, true}
}

// For driver container installs, check existence of .driver-ctr-ready to confirm running driver
// container has completed and is in Ready state.
func assertDriverContainerReady(silent, withWaitFlag bool) error {
	command := "bash"
	args := []string{"-c", "stat /run/nvidia/validations/.driver-ctr-ready"}

	if withWaitFlag {
		return runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent)
	}

	return runCommand(command, args, silent)
}

func (d *Driver) runValidation(silent bool) (string, bool, error, bool) {
	// driverChrootRoot, isHostDriver , nvidiaSMI, driverRoot, enableDevNodes := getDriverRoot()
	driverRoot := getDriverRoot()
	if !driverRoot.hostRoot {
		log.Infof("Driver is not pre-installed on the host. Checking driver container status.")
		if err := assertDriverContainerReady(silent, withWaitFlag); err != nil {
			return "", false, fmt.Errorf("error checking driver container status: %v", err), driverRoot.deviceNodes
		}
	}

	// invoke validation command
	command := "chroot"
	args := []string{driverRoot.driverChrootRoot, driverRoot.SMIcommand}

	if withWaitFlag {
		return driverRoot.driverContainerRoot, driverRoot.hostRoot, runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent), driverRoot.deviceNodes
	}

	return driverRoot.driverContainerRoot, driverRoot.hostRoot, runCommand(command, args, silent), driverRoot.deviceNodes
}

func (d *Driver) validate() error {
	// delete driver status file is already present
	err := deleteStatusFile(outputDirFlag + "/" + driverStatusFile)
	if err != nil {
		return err
	}

	// delete host driver status file is already present
	err = deleteStatusFile(outputDirFlag + "/" + hostDriverStatusFile)
	if err != nil {
		return err
	}

	driverRoot, isHostDriver, err, enableDevNodes := d.runValidation(false)
	if err != nil {
		log.Error("driver is not ready")
		return err
	}

	if !disableDevCharSymlinkCreation {
		log.Info("creating symlinks under /dev/char that correspond to NVIDIA character devices")
		err = createDevCharSymlinks(driverRoot, isHostDriver, enableDevNodes)
		if err != nil {
			msg := strings.Join([]string{
				"Failed to create symlinks under /dev/char that point to all possible NVIDIA character devices.",
				"The existence of these symlinks is required to address the following bug:",
				"",
				"    https://github.com/NVIDIA/gpu-operator/issues/430",
				"",
				"This bug impacts container runtimes configured with systemd cgroup management enabled.",
				"To disable the symlink creation, set the following envvar in ClusterPolicy:",
				"",
				"    validator:",
				"      driver:",
				"        env:",
				"        - name: DISABLE_DEV_CHAR_SYMLINK_CREATION",
				"          value: \"true\""}, "\n")
			return fmt.Errorf("%v\n\n%s", err, msg)
		}
	}

	statusFile := driverStatusFile
	if isHostDriver {
		statusFile = hostDriverStatusFile
	}

	// create driver status file
	err = createStatusFile(outputDirFlag + "/" + statusFile)
	if err != nil {
		return err
	}
	return nil
}

// createDevCharSymlinks creates symlinks in /host-dev-char that point to all possible NVIDIA devices nodes.
func createDevCharSymlinks(driverRoot string, isHostDriver bool, enableDevNodes bool) error {
	// If the host driver is being used, we rely on the fact that we are running a privileged container and as such
	// have access to /dev
	devRoot := driverRoot
	if isHostDriver {
		devRoot = "/"
	}
	// We now create the symlinks in /dev/char.
	creator, err := devchar.NewSymlinkCreator(
		devchar.WithDriverRoot(driverRoot),
		devchar.WithDevRoot(devRoot),
		devchar.WithDevCharPath(hostDevCharPath),
		devchar.WithCreateAll(true),
		devchar.WithCreateDeviceNodes(enableDevNodes),
		devchar.WithLoadKernelModules(enableDevNodes),
	)
	if err != nil {
		return fmt.Errorf("error creating symlink creator: %v", err)
	}

	err = creator.CreateLinks()
	if err != nil {
		return fmt.Errorf("error creating symlinks: %v", err)
	}

	return nil
}

func createStatusFile(statusFile string) error {
	_, err := os.Create(statusFile)
	if err != nil {
		return fmt.Errorf("unable to create status file %s: %s", statusFile, err)
	}
	return nil
}

func createStatusFileWithContent(statusFile string, content string) error {
	f, err := os.Create(statusFile)
	if err != nil {
		return fmt.Errorf("unable to create status file %s: %s", statusFile, err)
	}

	_, err = f.WriteString(content)
	if err != nil {
		return fmt.Errorf("unable to write contents of status file %s: %s", statusFile, err)
	}

	return nil
}

func deleteStatusFile(statusFile string) error {
	err := os.Remove(statusFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("unable to remove driver status file %s: %s", statusFile, err)
		}
		// status file already removed
	}
	return nil
}

func (n *NvidiaFs) validate() error {
	// delete driver status file if already present
	err := deleteStatusFile(outputDirFlag + "/" + nvidiaFsStatusFile)
	if err != nil {
		return err
	}

	err = n.runValidation(false)
	if err != nil {
		fmt.Println("nvidia-fs driver is not ready")
		return err
	}

	// create driver status file
	err = createStatusFile(outputDirFlag + "/" + nvidiaFsStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (n *NvidiaFs) runValidation(silent bool) error {
	// check for nvidia_fs module to be loaded
	command := "bash"
	args := []string{"-c", "lsmod | grep nvidia_fs"}

	if withWaitFlag {
		return runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent)
	}
	return runCommand(command, args, silent)
}

func (t *Toolkit) validate() error {
	// delete status file is already present
	err := deleteStatusFile(outputDirFlag + "/" + toolkitStatusFile)
	if err != nil {
		return err
	}

	// invoke nvidia-smi command to check if container run with toolkit injected files
	command := "nvidia-smi"
	args := []string{}
	if withWaitFlag {
		err = runCommandWithWait(command, args, sleepIntervalSecondsFlag, false)
	} else {
		err = runCommand(command, args, false)
	}
	if err != nil {
		fmt.Println("toolkit is not ready")
		return err
	}

	// create toolkit status file
	err = createStatusFile(outputDirFlag + "/" + toolkitStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (p *Plugin) validate() error {
	// delete status file is already present
	err := deleteStatusFile(outputDirFlag + "/" + pluginStatusFile)
	if err != nil {
		return err
	}

	// enumerate node resources and ensure GPU devices are discovered.
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Errorf("Error getting config cluster - %s\n", err.Error())
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Errorf("Error getting k8s client - %s\n", err.Error())
		return err
	}

	// update k8s client for the plugin
	p.setKubeClient(kubeClient)

	err = p.validateGPUResource()
	if err != nil {
		return err
	}

	if withWorkloadFlag {
		// workload test
		err = p.runWorkload()
		if err != nil {
			return err
		}
	}

	// create plugin status file
	err = createStatusFile(outputDirFlag + "/" + pluginStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (m *MOFED) validate() error {
	// If GPUDirectRDMA is disabled, skip validation
	if os.Getenv(GPUDirectRDMAEnabledEnvName) != "true" {
		log.Info("GPUDirect RDMA is disabled, skipping MOFED driver validation...")
		return nil
	}

	// Check node labels for Mellanox devices and MOFED driver status file
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Errorf("Error getting config cluster - %s\n", err.Error())
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Errorf("Error getting k8s client - %s\n", err.Error())
		return err
	}

	// update k8s client for the mofed driver validation
	m.setKubeClient(kubeClient)

	present, err := m.isMellanoxDevicePresent()
	if err != nil {
		log.Errorf(err.Error())
		return err
	}
	if !present {
		log.Info("No Mellanox device label found, skipping MOFED driver validation...")
		return nil
	}

	// delete status file is already present
	err = deleteStatusFile(outputDirFlag + "/" + mofedStatusFile)
	if err != nil {
		return err
	}

	err = m.runValidation(false)
	if err != nil {
		return err
	}

	// delete status file is already present
	err = createStatusFile(outputDirFlag + "/" + mofedStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (m *MOFED) runValidation(silent bool) error {
	// check for mlx5_core module to be loaded
	command := "bash"
	args := []string{"-c", "lsmod | grep mlx5_core"}

	// If MOFED container is running then use readiness flag set by the driver container instead
	if os.Getenv(UseHostMOFEDEnvname) != "true" {
		args = []string{"-c", "stat /run/mellanox/drivers/.driver-ready"}
	}
	if withWaitFlag {
		return runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent)
	}
	return runCommand(command, args, silent)
}

func (m *MOFED) setKubeClient(kubeClient kubernetes.Interface) {
	m.kubeClient = kubeClient
}

func (m *MOFED) isMellanoxDevicePresent() (bool, error) {
	node, err := getNode(m.ctx, m.kubeClient)
	if err != nil {
		return false, fmt.Errorf("unable to fetch node by name %s to check for Mellanox device label: %s", nodeNameFlag, err)
	}
	for key, value := range node.GetLabels() {
		if key == MellanoxDeviceLabelKey && value == "true" {
			return true, nil
		}
	}
	return false, nil
}

func (p *Plugin) runWorkload() error {
	ctx := p.ctx
	// load podSpec
	pod, err := loadPodSpec(pluginWorkloadPodSpecPath)
	if err != nil {
		return err
	}

	pod.ObjectMeta.Namespace = namespaceFlag
	image := os.Getenv(validatorImageEnvName)
	pod.Spec.Containers[0].Image = image
	pod.Spec.InitContainers[0].Image = image

	imagePullPolicy := os.Getenv(validatorImagePullPolicyEnvName)
	if imagePullPolicy != "" {
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullPolicy(imagePullPolicy)
		pod.Spec.InitContainers[0].ImagePullPolicy = corev1.PullPolicy(imagePullPolicy)
	}

	if os.Getenv(validatorImagePullSecretsEnvName) != "" {
		pullSecrets := strings.Split(os.Getenv(validatorImagePullSecretsEnvName), ",")
		for _, secret := range pullSecrets {
			pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: secret})
		}
	}
	if os.Getenv(validatorRuntimeClassEnvName) != "" {
		runtimeClass := os.Getenv(validatorRuntimeClassEnvName)
		pod.Spec.RuntimeClassName = &runtimeClass
	}

	// update owner reference
	err = setOwnerReference(ctx, p.kubeClient, pod)
	if err != nil {
		return fmt.Errorf("unable to set ownerReference for validator pod: %s", err)
	}

	// set pod tolerations
	err = setTolerations(ctx, p.kubeClient, pod)
	if err != nil {
		return fmt.Errorf("unable to set tolerations for validator pod: %s", err)
	}

	// update podSpec with node name so it will just run on current node
	pod.Spec.NodeName = nodeNameFlag

	resourceName, err := p.getGPUResourceName()
	if err != nil {
		return err
	}

	gpuResource := corev1.ResourceList{
		resourceName: resource.MustParse("1"),
	}

	pod.Spec.InitContainers[0].Resources.Limits = gpuResource
	pod.Spec.InitContainers[0].Resources.Requests = gpuResource
	opts := meta_v1.ListOptions{LabelSelector: labels.Set{"app": pluginValidatorLabelValue}.AsSelector().String(),
		FieldSelector: fields.Set{"spec.nodeName": nodeNameFlag}.AsSelector().String()}

	// check if plugin validation pod is already running and cleanup.
	podList, err := p.kubeClient.CoreV1().Pods(namespaceFlag).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("cannot list existing validation pods: %s", err)
	}

	if podList != nil && len(podList.Items) > 0 {
		propagation := meta_v1.DeletePropagationBackground
		gracePeriod := int64(0)
		options := meta_v1.DeleteOptions{PropagationPolicy: &propagation, GracePeriodSeconds: &gracePeriod}
		err = p.kubeClient.CoreV1().Pods(namespaceFlag).Delete(ctx, podList.Items[0].ObjectMeta.Name, options)
		if err != nil {
			return fmt.Errorf("cannot delete previous validation pod: %s", err)
		}
	}

	// wait for plugin validation pod to be ready.
	newPod, err := p.kubeClient.CoreV1().Pods(namespaceFlag).Create(ctx, pod, meta_v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create plugin validation pod %s, err %+v", pod.ObjectMeta.Name, err)
	}

	// make sure its available
	err = waitForPod(ctx, p.kubeClient, newPod.ObjectMeta.Name, namespaceFlag)
	if err != nil {
		return err
	}
	return nil
}

func setOwnerReference(ctx context.Context, kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	// get owner of validator daemonset (which is ClusterPolicy)
	validatorDaemonset, err := kubeClient.AppsV1().DaemonSets(namespaceFlag).Get(ctx, "nvidia-operator-validator", meta_v1.GetOptions{})
	if err != nil {
		return err
	}

	// update owner reference of plugin workload validation pod as ClusterPolicy for cleanup
	pod.SetOwnerReferences(validatorDaemonset.ObjectMeta.OwnerReferences)
	return nil
}

func setTolerations(ctx context.Context, kubeClient kubernetes.Interface, pod *corev1.Pod) error {
	// get tolerations of validator daemonset
	validatorDaemonset, err := kubeClient.AppsV1().DaemonSets(namespaceFlag).Get(ctx, "nvidia-operator-validator", meta_v1.GetOptions{})
	if err != nil {
		return err
	}

	// set same tolerations for individual validator pods
	pod.Spec.Tolerations = validatorDaemonset.Spec.Template.Spec.Tolerations
	return nil
}

// waits for the pod to be created
func waitForPod(ctx context.Context, kubeClient kubernetes.Interface, name string, namespace string) error {
	for i := 0; i < podCreationWaitRetries; i++ {
		// check for the existence of the resource
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(ctx, name, meta_v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod %s, err %+v", name, err)
		}
		if pod.Status.Phase != "Succeeded" {
			log.Infof("pod %s is curently in %s phase", name, pod.Status.Phase)
			time.Sleep(podCreationSleepIntervalSeconds * time.Second)
			continue
		}
		log.Infof("pod %s have run successfully", name)
		// successfully running
		return nil
	}
	return fmt.Errorf("gave up waiting for pod %s to be available", name)
}

func loadPodSpec(podSpecPath string) (*corev1.Pod, error) {
	var pod corev1.Pod
	manifest, err := os.ReadFile(podSpecPath)
	if err != nil {
		panic(err)
	}
	s := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme.Scheme,
		scheme.Scheme, json.SerializerOptions{Yaml: true, Pretty: false, Strict: false})
	reg := regexp.MustCompile(`\b(\w*kind:\w*)\B.*\b`)

	kind := reg.FindString(string(manifest))
	slice := strings.Split(kind, ":")
	kind = strings.TrimSpace(slice[1])

	log.Debugf("Decoding for Kind %s in path: %s", kind, podSpecPath)
	_, _, err = s.Decode(manifest, nil, &pod)
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

func (p *Plugin) countGPUResources() (int64, error) {
	// get node info to check discovered GPU resources
	node, err := getNode(p.ctx, p.kubeClient)
	if err != nil {
		return -1, fmt.Errorf("unable to fetch node by name %s to check for GPU resources: %s", nodeNameFlag, err)
	}

	count := int64(0)

	for resourceName, quantity := range node.Status.Capacity {
		if !strings.HasPrefix(string(resourceName), migGPUResourcePrefix) && !strings.HasPrefix(string(resourceName), genericGPUResourceType) {
			continue
		}

		count += quantity.Value()
	}
	return count, nil
}

func (p *Plugin) validateGPUResource() error {
	for retry := 1; retry <= gpuResourceDiscoveryWaitRetries; retry++ {
		// get node info to check discovered GPU resources
		node, err := getNode(p.ctx, p.kubeClient)
		if err != nil {
			return fmt.Errorf("unable to fetch node by name %s to check for GPU resources: %s", nodeNameFlag, err)
		}

		if p.availableMIGResourceName(node.Status.Capacity) != "" {
			return nil
		}

		if p.availableGenericResourceName(node.Status.Capacity) != "" {
			return nil
		}

		log.Infof("GPU resources are not yet discovered by the node, retry: %d", retry)
		time.Sleep(gpuResourceDiscoveryIntervalSeconds * time.Second)
	}
	return fmt.Errorf("GPU resources are not discovered by the node")
}

func (p *Plugin) availableMIGResourceName(resources corev1.ResourceList) corev1.ResourceName {
	for resourceName, quantity := range resources {
		if strings.HasPrefix(string(resourceName), migGPUResourcePrefix) && quantity.Value() >= 1 {
			log.Debugf("Found MIG GPU resource name %s quantity %d", resourceName, quantity.Value())
			return resourceName
		}
	}
	return ""
}

func (p *Plugin) availableGenericResourceName(resources corev1.ResourceList) corev1.ResourceName {
	for resourceName, quantity := range resources {
		if strings.HasPrefix(string(resourceName), genericGPUResourceType) && quantity.Value() >= 1 {
			log.Debugf("Found GPU resource name %s quantity %d", resourceName, quantity.Value())
			return resourceName
		}
	}
	return ""
}

func (p *Plugin) getGPUResourceName() (corev1.ResourceName, error) {
	// get node info to check allocatable GPU resources
	node, err := getNode(p.ctx, p.kubeClient)
	if err != nil {
		return "", fmt.Errorf("unable to fetch node by name %s to check for GPU resources: %s", nodeNameFlag, err)
	}

	// use mig resource if one is available to run workload
	if resourceName := p.availableMIGResourceName(node.Status.Allocatable); resourceName != "" {
		return resourceName, nil
	}

	if resourceName := p.availableGenericResourceName(node.Status.Allocatable); resourceName != "" {
		return resourceName, nil
	}

	return "", fmt.Errorf("Unable to find any allocatable GPU resource")
}

func (p *Plugin) setKubeClient(kubeClient kubernetes.Interface) {
	p.kubeClient = kubeClient
}

func getNode(ctx context.Context, kubeClient kubernetes.Interface) (*corev1.Node, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeNameFlag, meta_v1.GetOptions{})
	if err != nil {
		log.Errorf("unable to get node with name %s, err %s", nodeNameFlag, err.Error())
		return nil, err
	}
	return node, nil
}

func (c *CUDA) validate() error {
	// delete status file is already present
	err := deleteStatusFile(outputDirFlag + "/" + cudaStatusFile)
	if err != nil {
		return err
	}

	// deploy workload pod for cuda validation
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Errorf("Error getting config cluster - %s\n", err.Error())
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Errorf("Error getting k8s client - %s\n", err.Error())
		return err
	}

	// update k8s client for the plugin
	c.setKubeClient(kubeClient)

	if withWorkloadFlag {
		// workload test
		err = c.runWorkload()
		if err != nil {
			return err
		}
	}

	// create plugin status file
	err = createStatusFile(outputDirFlag + "/" + cudaStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (c *CUDA) setKubeClient(kubeClient kubernetes.Interface) {
	c.kubeClient = kubeClient
}

func (c *CUDA) runWorkload() error {
	ctx := c.ctx

	// load podSpec
	pod, err := loadPodSpec(cudaWorkloadPodSpecPath)
	if err != nil {
		return err
	}
	pod.ObjectMeta.Namespace = namespaceFlag
	image := os.Getenv(validatorImageEnvName)
	pod.Spec.Containers[0].Image = image
	pod.Spec.InitContainers[0].Image = image

	imagePullPolicy := os.Getenv(validatorImagePullPolicyEnvName)
	if imagePullPolicy != "" {
		pod.Spec.Containers[0].ImagePullPolicy = corev1.PullPolicy(imagePullPolicy)
		pod.Spec.InitContainers[0].ImagePullPolicy = corev1.PullPolicy(imagePullPolicy)
	}

	if os.Getenv(validatorImagePullSecretsEnvName) != "" {
		pullSecrets := strings.Split(os.Getenv(validatorImagePullSecretsEnvName), ",")
		for _, secret := range pullSecrets {
			pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: secret})
		}
	}
	if os.Getenv(validatorRuntimeClassEnvName) != "" {
		runtimeClass := os.Getenv(validatorRuntimeClassEnvName)
		pod.Spec.RuntimeClassName = &runtimeClass
	}

	// update owner reference
	err = setOwnerReference(ctx, c.kubeClient, pod)
	if err != nil {
		return fmt.Errorf("unable to set owner reference for validator pod: %s", err)
	}

	// set pod tolerations
	err = setTolerations(ctx, c.kubeClient, pod)
	if err != nil {
		return fmt.Errorf("unable to set tolerations for validator pod: %s", err)
	}

	// update podSpec with node name so it will just run on current node
	pod.Spec.NodeName = nodeNameFlag

	opts := meta_v1.ListOptions{LabelSelector: labels.Set{"app": cudaValidatorLabelValue}.AsSelector().String(),
		FieldSelector: fields.Set{"spec.nodeName": nodeNameFlag}.AsSelector().String()}

	// check if cuda workload pod is already running and cleanup.
	podList, err := c.kubeClient.CoreV1().Pods(namespaceFlag).List(ctx, opts)
	if err != nil {
		return fmt.Errorf("cannot list existing validation pods: %s", err)
	}

	if podList != nil && len(podList.Items) > 0 {
		propagation := meta_v1.DeletePropagationBackground
		gracePeriod := int64(0)
		options := meta_v1.DeleteOptions{PropagationPolicy: &propagation, GracePeriodSeconds: &gracePeriod}
		err = c.kubeClient.CoreV1().Pods(namespaceFlag).Delete(ctx, podList.Items[0].ObjectMeta.Name, options)
		if err != nil {
			return fmt.Errorf("cannot delete previous validation pod: %s", err)
		}
	}

	// wait for cuda workload pod to be ready.
	newPod, err := c.kubeClient.CoreV1().Pods(namespaceFlag).Create(ctx, pod, meta_v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create cuda validation pod %s, err %+v", pod.ObjectMeta.Name, err)
	}

	// make sure its available
	err = waitForPod(ctx, c.kubeClient, newPod.ObjectMeta.Name, namespaceFlag)
	if err != nil {
		return err
	}
	return nil
}

func (c *Metrics) run() error {
	m := NewNodeMetrics(c.ctx, metricsPort)

	return m.Run()
}

func (v *VfioPCI) validate() error {
	ctx := v.ctx

	gpuWorkloadConfig, err := getWorkloadConfig(ctx)
	if err != nil {
		return fmt.Errorf("Error getting gpu workload config: %s", err.Error())
	}
	log.Infof("GPU workload configuration: %s", gpuWorkloadConfig)

	err = createStatusFileWithContent(filepath.Join(outputDirFlag, workloadTypeStatusFile), gpuWorkloadConfig+"\n")
	if err != nil {
		return fmt.Errorf("Error updating %s status file: %v", workloadTypeStatusFile, err)
	}

	if gpuWorkloadConfig != gpuWorkloadConfigVMPassthrough {
		log.WithFields(log.Fields{
			"gpuWorkloadConfig": gpuWorkloadConfig,
		}).Info("vfio-pci not required on the node. Skipping validation.")
		return nil
	}

	// delete status file if already present
	err = deleteStatusFile(outputDirFlag + "/" + vfioPCIStatusFile)
	if err != nil {
		return err
	}

	err = v.runValidation(false)
	if err != nil {
		return err
	}
	log.Info("Validation completed successfully - all devices are bound to vfio-pci")

	// delete status file is already present
	err = createStatusFile(outputDirFlag + "/" + vfioPCIStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (v *VfioPCI) runValidation(silent bool) error {
	nvpci := nvpci.New()
	nvdevices, err := nvpci.GetGPUs()
	if err != nil {
		return fmt.Errorf("error getting NVIDIA PCI devices: %v", err)
	}

	for _, dev := range nvdevices {
		if dev.Driver != "vfio-pci" {
			return fmt.Errorf("device not bound to 'vfio-pci'; device: %s driver: '%s'", dev.Address, dev.Driver)
		}
	}

	return nil
}

func (v *VGPUManager) validate() error {
	ctx := v.ctx

	gpuWorkloadConfig, err := getWorkloadConfig(ctx)
	if err != nil {
		return fmt.Errorf("Error getting gpu workload config: %s", err.Error())
	}
	log.Infof("GPU workload configuration: %s", gpuWorkloadConfig)

	err = createStatusFileWithContent(filepath.Join(outputDirFlag, workloadTypeStatusFile), gpuWorkloadConfig+"\n")
	if err != nil {
		return fmt.Errorf("Error updating %s status file: %v", workloadTypeStatusFile, err)
	}

	if gpuWorkloadConfig != gpuWorkloadConfigVMVgpu {
		log.WithFields(log.Fields{
			"gpuWorkloadConfig": gpuWorkloadConfig,
		}).Info("vGPU Manager not required on the node. Skipping validation.")
		return nil
	}

	// delete status file if already present
	err = deleteStatusFile(outputDirFlag + "/" + vGPUManagerStatusFile)
	if err != nil {
		return err
	}

	// delete status file if already present
	err = deleteStatusFile(outputDirFlag + "/" + hostVGPUManagerStatusFile)
	if err != nil {
		return err
	}

	hostDriver, err := v.runValidation(false)
	if err != nil {
		fmt.Println("vGPU Manager is not ready")
		return err
	}

	statusFile := vGPUManagerStatusFile
	if hostDriver {
		statusFile = hostVGPUManagerStatusFile
	}

	// create driver status file
	err = createStatusFile(outputDirFlag + "/" + statusFile)
	if err != nil {
		return err
	}
	return nil
}

func (v *VGPUManager) runValidation(silent bool) (hostDriver bool, err error) {
	// invoke validation command
	command := "chroot"
	args := []string{"/run/nvidia/driver", "nvidia-smi"}

	// check if driver is pre-installed on the host and use host path for validation
	if _, err := os.Lstat("/host/usr/bin/nvidia-smi"); err == nil {
		args = []string{"/host", "nvidia-smi"}
		hostDriver = true
	}

	if withWaitFlag {
		return hostDriver, runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent)
	}

	return hostDriver, runCommand(command, args, silent)
}

func (c *CCManager) validate() error {
	// delete status file if already present
	err := deleteStatusFile(outputDirFlag + "/" + ccManagerStatusFile)
	if err != nil {
		return err
	}

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("Error getting cluster config - %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Errorf("Error getting k8s client - %s\n", err.Error())
		return err
	}

	// update k8s client for fetching node labels
	c.setKubeClient(kubeClient)

	err = c.runValidation(false)
	if err != nil {
		fmt.Println("CC Manager is not ready")
		return err
	}

	// create driver status file
	err = createStatusFile(outputDirFlag + "/" + ccManagerStatusFile)
	if err != nil {
		return err
	}
	return nil
}

func (c *CCManager) runValidation(silent bool) error {
	node, err := getNode(c.ctx, c.kubeClient)
	if err != nil {
		return fmt.Errorf("unable to fetch node by name %s to check for %s label: %s", nodeNameFlag, CCCapableLabelKey, err)
	}

	// make sure this is a CC capable node
	nodeLabels := node.GetLabels()
	if enabled, ok := nodeLabels[CCCapableLabelKey]; !ok || enabled != "true" {
		log.Info("Not a CC capable node, skipping CC Manager validation")
		return nil
	}

	// check if the ccManager container is ready
	err = assertCCManagerContainerReady(silent, withWaitFlag)
	if err != nil {
		return err
	}
	return nil
}

func (c *CCManager) setKubeClient(kubeClient kubernetes.Interface) {
	c.kubeClient = kubeClient
}

// Check that the ccManager container is ready after applying required ccMode
func assertCCManagerContainerReady(silent, withWaitFlag bool) error {
	command := "bash"
	args := []string{"-c", "stat /run/nvidia/validations/.cc-manager-ctr-ready"}

	if withWaitFlag {
		return runCommandWithWait(command, args, sleepIntervalSecondsFlag, silent)
	}

	return runCommand(command, args, silent)
}

func (v *VGPUDevices) validate() error {
	ctx := v.ctx

	gpuWorkloadConfig, err := getWorkloadConfig(ctx)
	if err != nil {
		return fmt.Errorf("Error getting gpu workload config: %s", err.Error())
	}
	log.Infof("GPU workload configuration: %s", gpuWorkloadConfig)

	err = createStatusFileWithContent(filepath.Join(outputDirFlag, workloadTypeStatusFile), gpuWorkloadConfig+"\n")
	if err != nil {
		return fmt.Errorf("Error updating %s status file: %v", workloadTypeStatusFile, err)
	}

	if gpuWorkloadConfig != gpuWorkloadConfigVMVgpu {
		log.WithFields(log.Fields{
			"gpuWorkloadConfig": gpuWorkloadConfig,
		}).Info("vgpu devices not required on the node. Skipping validation.")
		return nil
	}

	// delete status file if already present
	err = deleteStatusFile(outputDirFlag + "/" + vGPUDevicesStatusFile)
	if err != nil {
		return err
	}

	err = v.runValidation(false)
	if err != nil {
		return err
	}
	log.Info("Validation completed successfully - vGPU devices present on the host")

	// create status file
	err = createStatusFile(outputDirFlag + "/" + vGPUDevicesStatusFile)
	if err != nil {
		return err
	}

	return nil
}

func (v *VGPUDevices) runValidation(silent bool) error {
	nvmdev := nvmdev.New()
	vGPUDevices, err := nvmdev.GetAllDevices()
	if err != nil {
		return fmt.Errorf("Error checking for vGPU devices on the host: %v", err)
	}

	if !withWaitFlag {
		numDevices := len(vGPUDevices)
		if numDevices == 0 {
			return fmt.Errorf("No vGPU devices found")
		}

		log.Infof("Found %d vGPU devices", numDevices)
		return nil
	}

	for {
		numDevices := len(vGPUDevices)
		if numDevices > 0 {
			log.Infof("Found %d vGPU devices", numDevices)
			return nil
		}
		log.Infof("No vGPU devices found, retrying after %d seconds", sleepIntervalSecondsFlag)
		time.Sleep(time.Duration(sleepIntervalSecondsFlag) * time.Second)

		vGPUDevices, err = nvmdev.GetAllDevices()
		if err != nil {
			return fmt.Errorf("Error checking for vGPU devices on the host: %v", err)
		}
	}
}
