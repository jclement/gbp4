package main

import (
	"flag"
	"github.com/BurntSushi/toml"
	"github.com/eclipse/paho.mqtt.golang"
	"github.com/op/go-logging"
	"github.com/stianeikeland/go-rpio"
	"time"
)

// GarageControllerConfig represents all startup configuration for a garage controller
type GarageControllerConfig struct {
	MQTTServer        string
	MQTTUsername      string
	MQTTPassword      string
	MQTTClientID      string
	MQTTTopicPresence string
	MQTTTopicControl  string
	MQTTTopicStatus   string
	TravelDelay       int
	PinStatus         int
	PinControl        int
}

// DoorState represents the state of the garage door
type DoorState uint8

// Various door states
const (
	DoorOpen DoorState = iota
	DoorClosed
	DoorOpening
	DoorClosing
	DoorUnknown
)

func (state DoorState) String() string {
	switch state {
	case DoorOpen:
		return "Open"
	case DoorClosed:
		return "Closed"
	case DoorOpening:
		return "Opening"
	case DoorClosing:
		return "Closing"
	}
	return "Unknown"
}

// GarageController handles door state and messaging to MQTT
type GarageController struct {
	currentDoorState    DoorState
	movingStatus        bool
	client              mqtt.Client
	config              GarageControllerConfig
	log                 *logging.Logger
	movingStatusChannel chan bool      // channel for communicating if door is still movingStatus
	statusChannel       chan DoorState // channel for communicating current status of door
	pinStatus           rpio.Pin
	pinControl          rpio.Pin
}

// NewGarageController creates a new Garage Controller from a configuration and logging object
func NewGarageController(config GarageControllerConfig, log *logging.Logger) *GarageController {

	pinStatus := rpio.Pin(config.PinStatus)
	pinStatus.Mode(rpio.Input)
	pinStatus.Pull(rpio.PullDown)

	pinControl := rpio.Pin(config.PinControl)
	pinControl.Mode(rpio.Output)

	controller := GarageController{
		config:              config,
		movingStatus:        false,
		log:                 log,
		currentDoorState:    DoorUnknown,
		movingStatusChannel: make(chan bool),
		statusChannel:       make(chan DoorState),
		pinControl:          pinControl,
		pinStatus:           pinStatus,
	}

	clientOptions := mqtt.NewClientOptions()
	clientOptions.AddBroker(config.MQTTServer)
	clientOptions.SetUsername(config.MQTTUsername)
	clientOptions.SetPassword(config.MQTTPassword)
	clientOptions.SetClientID(config.MQTTClientID)
	clientOptions.SetAutoReconnect(true)
	clientOptions.SetWill(config.MQTTTopicPresence, "GBP-OFFLINE", 0, false)
	clientOptions.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		if msg.Topic() == config.MQTTTopicControl && !controller.movingStatus {
			switch string(msg.Payload()) {
			case "O":
				controller.OpenDoor()
			case "C":
				controller.CloseDoor()
			}
		}
	})

	controller.client = mqtt.NewClient(clientOptions)
	if token := controller.client.Connect(); token.Wait() && token.Error() != nil {
		controller.log.Fatalf("Unable to connect to '%s': %v", config.MQTTServer, token.Error())
	}

	if token := controller.client.Publish(config.MQTTTopicPresence, 0, false, "GBP-ONLINE"); token.Wait() && token.Error() != nil {
		controller.log.Fatalf("Unable to publish presence '%s': %v", config.MQTTServer, token.Error())
	}

	if token := controller.client.Subscribe(config.MQTTTopicControl, 0, nil); token.Wait() && token.Error() != nil {
		controller.log.Fatalf("Unable to subscribe to control topic '%s': %v", config.MQTTTopicControl, token.Error())
	}

	controller.setState(controller.readState())

	return &controller
}

// readState pulls the status of the garage door unless the door is currently movingStatus.  Then we return the current status
func (controller *GarageController) readState() DoorState {
	if controller.movingStatus {
		return controller.currentDoorState
	}

	if rpio.ReadPin(controller.pinStatus) == rpio.High {
		return DoorOpen
	}

	return DoorClosed
}

func (controller *GarageController) setState(newState DoorState) {
	if controller.currentDoorState == newState {
		return
	}
	controller.log.Infof("Door state from %v to %v", controller.currentDoorState, newState)
	controller.currentDoorState = newState
	switch controller.currentDoorState {
	case DoorOpen:
		controller.client.Publish(controller.config.MQTTTopicStatus, 0, true, "O")
	case DoorClosed:
		controller.client.Publish(controller.config.MQTTTopicStatus, 0, true, "C")
	case DoorOpening:
		controller.client.Publish(controller.config.MQTTTopicStatus, 0, true, "U")
	case DoorClosing:
		controller.client.Publish(controller.config.MQTTTopicStatus, 0, true, "D")
	}
}

func (controller *GarageController) toggleDoor() {
	controller.log.Debug("Toggling door")
	rpio.WritePin(controller.pinControl, rpio.High)
	time.Sleep(250 * time.Millisecond)
	rpio.WritePin(controller.pinControl, rpio.Low)
}

// OpenDoor opens the door
func (controller *GarageController) OpenDoor() {
	if controller.currentDoorState != DoorClosed {
		return
	}
	controller.log.Info("Opening Door")
	controller.movingStatus = true
	controller.setState(DoorOpening)
	controller.toggleDoor()
	go func(ch chan<- bool) {
		time.Sleep(time.Duration(controller.config.TravelDelay) * time.Second)
		ch <- false
	}(controller.movingStatusChannel)
}

// CloseDoor closes the door
func (controller *GarageController) CloseDoor() {
	if controller.currentDoorState != DoorOpen {
		return
	}
	controller.log.Info("Closing Door")
	controller.movingStatus = true
	controller.setState(DoorClosing)
	controller.toggleDoor()
	go func(ch chan<- bool) {
		time.Sleep(time.Duration(controller.config.TravelDelay) * time.Second)
		ch <- false
	}(controller.movingStatusChannel)
}

// Run invoke the main controller loop
func (controller *GarageController) Run() {

	// main loop
	for {

		// if the door isn't moving, check the status
		if !controller.movingStatus {
			controller.setState(controller.readState())
		}

		select {
		case moving := <-controller.movingStatusChannel:
			controller.movingStatus = moving
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func main() {
	if err := rpio.Open(); err != nil {
		panic(err)
	}
	defer rpio.Close()

	log := logging.MustGetLogger("loader")

	configPath := flag.String("c", "controller.config", "Controller configuration file")
	flag.Parse()

	var config GarageControllerConfig
	if _, err := toml.DecodeFile(*configPath, &config); err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	controller := NewGarageController(config, log)
	controller.Run()
}
