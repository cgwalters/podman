//go:build amd64 || arm64

package bootc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containers/podman/v4/cmd/podman/common"
	cmdmachine "github.com/containers/podman/v4/cmd/podman/machine"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/pkg/machine"
	machinevirtprovider "github.com/containers/podman/v4/pkg/machine/provider"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// TODO get this from the container
const diskSize = 10 * 1024 * 1024 * 1024

// vmrunCacheDir is placed under the machine dir
const vmrunCacheDir = "bootc-vmrun"

// imageMetaXattr holds serialized diskFromContainerMeta
const imageMetaXattr = "user.bootc.meta"

// loopWrapperEntrypoint sets up a loopback device
const loopWrapperEntrypoint = `#!/bin/bash
set -euo pipefail
disk=$1
shift
set -x
dev=$(losetup --show -P -f "$disk")
# TODO consider just adding to-disk --with-loopback
rc=0
# FIXME detect console from the environment and OS...see also
# https://github.com/coreos/fedora-coreos-config/pull/2785
# https://github.com/coreos/fedora-coreos-config/blob/8d5c7cd1bbc82d6af7af1b64571d2317eeab9347/platforms.yaml
set +e
bootc install to-disk --karg console=hvc0 --karg console=ttyS0,114800n8 --karg console=tty0 \
  --skip-fetch-check --generic-image --via-loopback $1
rc=$?
losetup -d /dev/loop0
exit $rc
`

// diskFromContainerMeta is serialized to JSON in a user xattr on a disk image
type diskFromContainerMeta struct {
	// imageDigest is the digested sha256 of the container that was used to build this disk
	ImageDigest string `json:"imageDigest"`
}

type vmRunCtx struct {
	cmd      *cobra.Command
	cachedir string
}

type optionsData struct {
	bootcLogLevel string
	vmDebug       bool
}

var (
	vmrunCommand = &cobra.Command{
		Use:               "vmrun [options] IMAGE",
		Short:             "Start a transient virtual machine from a bootc-enabled container image",
		Args:              cobra.ExactArgs(1),
		RunE:              vmrun,
		ValidArgsFunction: common.AutocompleteImages,
		Example:           `podman machine bootc vmrun quay.io/exampleos/someos:latest`,
	}
	options optionsData
)

func init() {
	flags := vmrunCommand.Flags()
	flags.StringVar(&options.bootcLogLevel, "bootc-log-level", "", "Enable bootc install debugging")
	flags.BoolVar(&options.vmDebug, "vmdebug", false, "Enable debugging for VM launching")
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: vmrunCommand,
		Parent:  cmdmachine.BootcCommand,
	})
}

var (
	vmprovider machine.VirtProvider
)

func podmanRecurse(cmd *cobra.Command, args []string) *exec.Cmd {
	// Yes, we're just executing ourself as a subprocess.  I wrote a lot of
	// copy-paste code initially using the internal Go APIs, but it's WAY more verbose for...not
	// much immediate value right now, though *in theory* we should be honoring
	// some CLI options...for example --log-level.  Right now we just cherry pick --connection.
	connection := cmd.Root().Flag("connection").Value.String()

	fullArgs := []string{"--connection=" + connection}
	fullArgs = append(fullArgs, args...)
	c := exec.Command("podman", fullArgs...)
	return c
}

func podmanRecurseRun(cmd *cobra.Command, args []string) error {
	c := podmanRecurse(cmd, args)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func createDiskImage(ctx *vmRunCtx, imageName, imageDigest, targetDisk string) (string, error) {
	temporaryDisk, err := os.CreateTemp(ctx.cachedir, "podman-bootc-tempdisk")
	if err != nil {
		return "", err
	}
	if err := syscall.Ftruncate(int(temporaryDisk.Fd()), diskSize); err != nil {
		return "", err
	}
	doCleanupDisk := true
	defer func() {
		if doCleanupDisk {
			os.Remove(temporaryDisk.Name())
		}
	}()

	temporaryEntrypoint, err := os.CreateTemp(ctx.cachedir, "entrypoint")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporaryEntrypoint.Name())
	if _, err := io.Copy(temporaryEntrypoint, strings.NewReader(loopWrapperEntrypoint)); err != nil {
		return "", err
	}
	if err := temporaryEntrypoint.Chmod(0o755); err != nil {
		return "", err
	}

	logrus.Info("Generating disk image")
	genDiskArgs := []string{"run", "--privileged", "--pid=host", "--rm", "-i", "--security-opt=label=type:unconfined_t"}
	if options.bootcLogLevel != "" {
		genDiskArgs = append(genDiskArgs, "--env=RUST_LOG="+options.bootcLogLevel)
	}
	tempDiskInContainer := "/disk"
	tempEntrypointInContainer := "/entrypoint"
	genDiskArgs = append(genDiskArgs, "-v", fmt.Sprintf("%s:%s", temporaryDisk.Name(), tempDiskInContainer))
	genDiskArgs = append(genDiskArgs, "-v", fmt.Sprintf("%s:%s", temporaryEntrypoint.Name(), tempEntrypointInContainer))
	genDiskArgs = append(genDiskArgs, imageName)
	genDiskArgs = append(genDiskArgs, "/bin/bash", tempEntrypointInContainer, tempDiskInContainer)
	logrus.Infof("Executing podman %s", genDiskArgs)
	if err := podmanRecurseRun(ctx.cmd, genDiskArgs); err != nil {
		return "", fmt.Errorf("failed to run container to generate temporary disk: %w", err)
	}

	doCleanupDisk = false
	serializedMeta := diskFromContainerMeta{
		ImageDigest: imageDigest,
	}
	buf, err := json.Marshal(serializedMeta)
	if err != nil {
		return "", err
	}
	if err := unix.Fsetxattr(int(temporaryDisk.Fd()), imageMetaXattr, buf, 0); err != nil {
		return "", fmt.Errorf("failed to set xattr: %w", err)
	}
	if err := os.Rename(temporaryDisk.Name(), targetDisk); err != nil {
		return "", fmt.Errorf("failed to rename to %s: %w", targetDisk, err)
	}
	return targetDisk, nil
}

func getOrCreateDiskImage(ctx *vmRunCtx, imageName, imageDigest string) (string, error) {
	diskImageName := strings.ReplaceAll(imageName, "/", "_")
	diskPath := filepath.Join(ctx.cachedir, diskImageName)
	f, err := os.Open(diskPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	defer f.Close()
	buf := make([]byte, 4096)
	len, err := unix.Fgetxattr(int(f.Fd()), imageMetaXattr, buf)
	if err != nil {
		// If there's no xattr, just remove it
		os.Remove(diskPath)
		return createDiskImage(ctx, imageName, imageDigest, diskPath)
	}
	bufTrimmed := buf[:len]
	var serializedMeta diskFromContainerMeta
	if err := json.Unmarshal(bufTrimmed, &serializedMeta); err != nil {
		logrus.Warnf("failed to parse serialized meta from %s (%v) %v", diskPath, buf, err)
		return createDiskImage(ctx, imageName, imageDigest, diskPath)
	}

	logrus.Debugf("previous disk digest: %s current digest: %s", serializedMeta.ImageDigest, imageDigest)
	if serializedMeta.ImageDigest == imageDigest {
		return diskPath, nil
	}

	return createDiskImage(ctx, imageName, imageDigest, diskPath)
}

func vmrun(cmd *cobra.Command, args []string) error {
	// machine unsets the root pre-run; we want to re-enable it
	if err := cmd.Root().PersistentPreRunE(cmd, args); err != nil {
		return err
	}

	datadir, err := machine.GetGlobalDataDir()
	if err != nil {
		return err
	}

	vmcachedir := filepath.Join(datadir, vmrunCacheDir)
	if err := os.MkdirAll(vmcachedir, 0o755); err != nil {
		return err
	}

	ctx := vmRunCtx{
		cmd:      cmd,
		cachedir: vmcachedir,
	}

	imageName := args[0]

	// Run an inspect to see if the image is present, otherwise pull.
	// TODO: Add podman pull --if-not-present or so.
	c := podmanRecurse(cmd, []string{"image", "inspect", "-f", "{{.Digest}}", imageName})
	if err := c.Run(); err != nil {
		logrus.Debugf("Inspect failed: %v", err)
		if err := podmanRecurseRun(cmd, []string{"pull", imageName}); err != nil {
			return err
		}
	}

	c = podmanRecurse(cmd, []string{"image", "inspect", "-f", "{{.Digest}}", imageName})
	buf := &bytes.Buffer{}
	c.Stdout = buf
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("failed to inspect %s: %w", imageName, err)
	}
	digest := strings.TrimSpace(buf.String())

	disk, err := getOrCreateDiskImage(&ctx, imageName, digest)
	if err != nil {
		return err
	}

	fmt.Printf("generated %s\n", disk)

	// Create a cloned copy of the disk
	diskdir := filepath.Dir(disk)
	tempf, err := os.CreateTemp(diskdir, "bootc-vmrun")
	if err != nil {
		return err
	}
	defer os.Remove(tempf.Name())
	tempf.Close()
	if err := exec.Command("cp", "-fc", disk, tempf.Name()).Run(); err != nil {
		return err
	}

	vmprovider, err := machinevirtprovider.Get()
	if err != nil {
		return err
	}

	spawnopts := machine.SpawnTransientOpts{
		Cpus:      2,
		MemoryMiB: 2048,
		Disk:      disk,
		Gui:       true,
		VMDebug:   options.vmDebug,
	}

	return vmprovider.SpawnTransient(spawnopts)
}
