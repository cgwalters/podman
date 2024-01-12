//go:build amd64 || arm64

package machine

import (
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/validate"
	"github.com/spf13/cobra"
)

var (
	BootcCommand = &cobra.Command{
		Use:   "bootc",
		Short: "Interact with bootc containers and virtual machines",
		// For now
		Hidden: true,
		RunE:   validate.SubCommandExists,
	}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: BootcCommand,
		Parent:  machineCmd,
	})
}
