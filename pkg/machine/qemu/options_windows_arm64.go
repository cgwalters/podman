package qemu

var (
	QemuCommand = "qemu-system-aarch64w"
)

func addArchOptions(_ string, _ *setNewMachineCMDOpts) []string {
	// stub to fix compilation issues
	opts := []string{}
	return opts
}

func (v *MachineVM) prepare() error {
	return nil
}

func (v *MachineVM) archRemovalFiles() []string {
	return []string{}
}
