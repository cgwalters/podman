package qemu

var (
	QemuCommand = "qemu-system-x86_64"
)

func addArchOptions(_ string, _ *setNewMachineCMDOpts) []string {
	opts := []string{"-machine", "q35,accel=hvf:tcg", "-cpu", "host"}
	return opts
}

func (v *MachineVM) prepare() error {
	return nil
}

func (v *MachineVM) archRemovalFiles() []string {
	return []string{}
}
