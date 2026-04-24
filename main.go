package main

import (
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"voice-agent/airesponder"
	"voice-agent/audiototext"
	"voice-agent/router"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: audiototext.Model},
		resource.APIModel{API: generic.API, Model: router.Model},
		resource.APIModel{API: generic.API, Model: airesponder.Model},
	)
}
