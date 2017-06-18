package main

import (
	"flag"

	"github.com/BurntSushi/toml"
	"github.com/brutella/hc"
	"github.com/brutella/hc/accessory"
	"github.com/brutella/hc/characteristic"
	"github.com/brutella/hc/service"
	hklog "github.com/brutella/log"
	"github.com/op/go-logging"
	"github.com/eclipse/paho.mqtt.golang"
)

type HomeKitConfig struct {
	MQTTServer        string
	MQTTUsername      string
	MQTTPassword      string
	MQTTClientID      string
	MQTTTopicPresence string
	MQTTTopicControl  string
	MQTTTopicStatus   string
	HomeKitName       string
	HomeKitPIN        string
	HomeKitSerial     string
}

type HomeKitController struct {
	Log       *logging.Logger
	Config    HomeKitConfig
	Accessory *accessory.Accessory
	Opener    *service.GarageDoorOpener
	Client mqtt.Client
}

func (controller *HomeKitController) Start() {
	if token := controller.Client.Connect(); token.Wait() && token.Error() != nil {
		controller.Log.Fatalf("Unable to connect to '%s': %v", controller.Config.MQTTServer, token.Error())
	}

	if token := controller.Client.Publish(controller.Config.MQTTTopicPresence, 0, false, "GBP-HOMEKIT-ONLINE"); token.Wait() && token.Error() != nil {
		controller.Log.Fatalf("Unable to publish presence '%s': %v", controller.Config.MQTTServer, token.Error())
	}

	if token := controller.Client.Subscribe(controller.Config.MQTTTopicStatus, 0, nil); token.Wait() && token.Error() != nil {
		controller.Log.Fatalf("Unable to subscribe to status topic '%s': %v", controller.Config.MQTTTopicStatus, token.Error())
	}

	t, err := hc.NewIPTransport(hc.Config{Pin: controller.Config.HomeKitPIN}, controller.Accessory)
	if err != nil {
		controller.Log.Fatal(err)
	}

	hc.OnTermination(t.Stop)
	t.Start()
}

func NewHomeKitController(config HomeKitConfig, log *logging.Logger) *HomeKitController {
	info := accessory.Info{
		Name:         config.HomeKitName,
		Manufacturer: "Jeff Clement",
		Model:        "GarageberryPi",
		SerialNumber: config.HomeKitSerial,
	}

	controller := HomeKitController{
		Config:    config,
		Log:       log,
		Accessory: accessory.New(info, accessory.TypeGarageDoorOpener),
		Opener:    service.NewGarageDoorOpener(),
		Client:    nil,
	}

	controller.Accessory.AddService(controller.Opener.Service)
	controller.Opener.TargetDoorState.OnValueRemoteUpdate(func(value int) {

		code := ""
		switch value {
		case characteristic.TargetDoorStateOpen:
			code = "O"
			controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpen)
		case characteristic.TargetDoorStateClosed:
			code = "C"
			controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosed)
		}

		if code != "" {
			controller.Log.Debugf("Updating to %s", code)
			if token := controller.Client.Publish(config.MQTTTopicControl, 0, false, code); token.Wait() && token.Error() != nil {
				controller.Log.Fatalf("Unable to publish control message : %v", token.Error())
			}
		}

	})

	clientOptions := mqtt.NewClientOptions()
	clientOptions.AddBroker(config.MQTTServer)
	clientOptions.SetUsername(config.MQTTUsername)
	clientOptions.SetPassword(config.MQTTPassword)
	clientOptions.SetClientID(config.MQTTClientID)
	clientOptions.SetAutoReconnect(true)
	clientOptions.SetWill(config.MQTTTopicPresence, "GBP-HOMEKIT-OFFLINE", 0, false)
	clientOptions.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		if msg.Topic() == config.MQTTTopicStatus {
			controller.Log.Debugf("New status: %s", string(msg.Payload()))
			switch string(msg.Payload()) {
			case "O":
				controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpen)
			case "U":
				controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateOpening)
			case "D":
				controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosing)
			case "C":
				controller.Opener.CurrentDoorState.SetValue(characteristic.CurrentDoorStateClosed)
			}
		}
	})

	controller.Client = mqtt.NewClient(clientOptions)

	return &controller
}

func main() {
	hklog.Verbose = true
	hklog.Info = true

	log := logging.MustGetLogger("loader")

	configPath := flag.String("c", "homekit.config", "HomeKit configuration file")
	flag.Parse()

	var config HomeKitConfig
	if _, err := toml.DecodeFile(*configPath, &config); err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	controller := NewHomeKitController(config, log)
	controller.Start()

}
