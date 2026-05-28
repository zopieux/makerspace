package usb

import "fmt"

type UblkDevice interface {
	Close() error
	DevPath() string
}

type Orchestrator interface {
	SwitchUSBMode(mode USBMode) error
	GadgetInit(cfg GadgetConfig) error
	GadgetClose()
	IsBound() bool
	Detach()
	UblkUpdate(sourceDir, label string) (UblkDevice, error)
}

type RealOrchestrator struct{}

func (r *RealOrchestrator) SwitchUSBMode(mode USBMode) error {
	return SwitchUSBMode(mode)
}

func (r *RealOrchestrator) GadgetInit(cfg GadgetConfig) error {
	return GadgetInit(cfg)
}

func (r *RealOrchestrator) GadgetClose() {
	GadgetClose()
}

func (r *RealOrchestrator) IsBound() bool {
	return IsBound()
}

func (r *RealOrchestrator) Detach() {
	Detach()
}

func (r *RealOrchestrator) UblkUpdate(sourceDir, label string) (UblkDevice, error) {
	srv := UblkUpdate(sourceDir, label)
	if srv == nil {
		return nil, fmt.Errorf("ublk server failed to start")
	}
	return srv, nil
}
