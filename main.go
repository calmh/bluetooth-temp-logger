package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/photostorm/gatt"
	"github.com/photostorm/gatt/examples/option"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	airTemp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "btl",
		Subsystem: "sensorbug",
		Name:      "temperature_c",
	}, []string{"unit"})
	battery = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "btl",
		Subsystem: "sensorbug",
		Name:      "battery_percent",
	}, []string{"unit"})
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

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(":9298", nil); err != nil {
			log.Fatalln("HTTP listen:", err)
		}
	}()

	log.Println("Running")
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
	battery.WithLabelValues(p.ID()).Set(float64(batt))

	var str strings.Builder
	fmt.Fprintf(&str, "batt:%d%%", batt)

	rest := a.ManufacturerData[7:]
	// fmt.Fprintf(&str, " manuf:%x", rest)
	for len(rest) > 0 {
		dataType := rest[0] & 0b00_111111
		hasData := rest[0]&0b01_000000 != 0
		hasAlert := rest[0]&0b10_000000 != 0
		rest = rest[1:]

		if hasAlert {
			rest = rest[1:]
		}
		if !hasData {
			continue
		}

		switch dataType {
		case 0x01:
			// Accellerometer
			// info about alerts only, uninteresting
			rest = rest[2:]

		case 0x02:
			// Light
			isIR := rest[0]&0b1_0_00_00_00 != 0
			dataResolution := rest[0] & 0b0_0_11_00_00 >> 4
			dataRange := rest[0] & 0b0_0_00_11_00 >> 2
			dataLen := rest[0] & 0b0_0_00_00_11
			var data uint16
			if dataLen == 2 {
				data = binary.LittleEndian.Uint16(rest[1:])
			} else {
				data = uint16(rest[1])
			}
			fmt.Fprintf(&str, " light:%v/%d/%d/%d", isIR, dataResolution, dataRange, data)
			rest = rest[1+int(dataLen):]

		case 0x03:
			// Temperature
			temp := 0.0625 * float64(int16(binary.LittleEndian.Uint16(rest)))
			fmt.Fprintf(&str, " temp:%.01f°C", temp)
			airTemp.WithLabelValues(p.ID()).Set(temp)
			rest = rest[2:]

		case 0x2f:
			// Pairing, don't case
			rest = rest[1:]

		case 0x3f:
			// Encryption pairing, we're done
			rest = nil
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
