package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/photostorm/gatt"
	"github.com/photostorm/gatt/examples/option"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(0)

	d, err := gatt.NewDevice(option.DefaultServerOptions...)
	if err != nil {
		log.Fatalln("Failed to open device:", err)
	}

	s := newState()

	d.Handle(gatt.PeripheralDiscovered(func(p gatt.Peripheral, a *gatt.Advertisement, rssi int) {
		s.disco <- discovery{p, a, rssi}
	}))

	if err := d.Init(onStateChanged); err != nil {
		log.Fatalln("Failed to init device:", err)
	}

	s.serve()

	d.StopScanning()
	d.Stop()
}

func onStateChanged(d gatt.Device, s gatt.State) {
	log.Println("State:", s)
	switch s {
	case gatt.StatePoweredOn:
		log.Println("scanning...")
		d.Scan([]gatt.UUID{}, true)
		return
	default:
		log.Println("Stopping scan")
		d.StopScanning()
	}
}

type state struct {
	updates map[string]*update
	disco   chan discovery
}

type update struct {
	message string
	changed bool
}

type discovery struct {
	periph gatt.Peripheral
	advert *gatt.Advertisement
	rssi   int
}

func newState() *state {
	return &state{
		updates: make(map[string]*update),
		disco:   make(chan discovery, 16),
	}
}

func (s *state) serve() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case disco := <-s.disco:
			s.onDiscovery(disco.periph, disco.advert, disco.rssi)
		case <-ticker.C:
			for id, update := range s.updates {
				if update.changed {
					log.Printf("%s: %s\n", id, update.message)
					update.changed = false
				}
			}
		case <-sigs:
			log.Println("Exit on interrupt")
			return
		}
	}
}

func (s *state) onDiscovery(p gatt.Peripheral, a *gatt.Advertisement, rssi int) {
	if len(a.ManufacturerData) < 7 {
		return
	}
	if !bytes.Equal(a.ManufacturerData[:5], []byte{0x85, 0x00, 0x02, 0x00, 0x3c}) {
		return
	}

	batt := int(a.ManufacturerData[5])

	var str strings.Builder
	fmt.Fprintf(&str, "batt:%d%%", batt)

	rest := a.ManufacturerData[7:]
	if len(rest) > 0 {
		switch rest[0] & 0x3f {
		case 3:
			// Temperature
			fmt.Fprintf(&str, " temp:%.01f°C", 0.0625*float64(rest[1]))
		}
	}

	res := str.String()
	cur := s.updates[p.ID()]
	if cur == nil {
		cur = &update{}
		s.updates[p.ID()] = cur
		log.Printf("%s: new: %s\n", p.ID(), res)
	}
	if cur.message != res {
		cur.message = res
		cur.changed = true
	}
}
