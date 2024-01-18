package qemu

var (
	QemuCommand = "qemu-system-aarch64"
)

func addArchOptions(_ string, _ *setNewMachineCMDOpts) []string {
	opts := []string{
		"-machine", "virt",
		"-accel", "tcg",
		"-cpu", "host"}
	return opts
}

func (v *MachineVM) prepare() error {
	return nil
}

func (v *MachineVM) archRemovalFiles() []string {
	return []string{}
}
