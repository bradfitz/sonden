// Copyright 2011 Google Inc.
// See LICENSE file.
//
// Home automation & power saving daemon.
//
// Listens to audio input and turns on my Denon receivers when there's
// music and turns them off to save power when things are silent for
// awhile.
//
// Author: Brad Fitzpatrick <brad@danga.com>
//

package main

import (
	"encoding/binary"
	"flag"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go-avr/avr"
)

// Flags
var (
	ampAddrs = flag.String("amps", "", "Comma-separated list of ip:port of Denon amps")
	idleSec  = flag.Int("idlesec", 300, "number of seconds of silence before turning off amps")
)

const (
	quietVarianceThreshold = 1000 // typically ~2. occasionally as high as 16.
	sampleHz               = 8 << 10
	ringSize               = sampleHz // 2 seconds of audio
)

type sampleRing struct {
	i       int
	size    int
	samples [ringSize]int16
	sum     int
}

func (r *sampleRing) Add(sample int16) {
	if r.size == len(r.samples) {
		r.sum -= int(r.samples[r.i])
	} else {
		r.size++
	}
	r.sum += int(sample)
	r.samples[r.i] = sample
	r.i++
	if r.i == len(r.samples) {
		r.i = 0
	}
}

func (r *sampleRing) Variance() float64 {
	mean := float64(r.sum) / float64(r.size)
	v := 0.0
	for _, sample := range r.samples {
		v += math.Pow(math.Abs(float64(mean)-float64(sample)), 2)
	}
	v /= float64(r.size)
	return v
}

func setAmpState(amp *avr.Amp, state bool) {
	cmds := []string{"ZMOFF", "PWSTANDBY"}
	if state {
		cmds = []string{"ZMON", "PWON"}
	}
	for _, cmd := range cmds {
		log.Printf("Sending command %q", cmd)
		err := amp.SendCommand(cmd)
		if err != nil {
			log.Printf("Sendind command %q failed: %v", cmd, err)
			return
		}
	}
}

func main() {
	flag.Parse()

	amps := []*avr.Amp{}
	for _, addr := range strings.Split(*ampAddrs, ",") {
		amps = append(amps, avr.New(addr))
	}

	cmd := exec.Command("rec",
		"-t", "raw",
		"--endian", "little",
		"-r", strconv.Itoa(sampleHz),
		"-e", "signed",
		"-b", "16", // 16 bits per sample
		"-c", "1", // one channel
		"-")
	out, _ := cmd.StdoutPipe()
	err := cmd.Start()
	if err != nil {
		log.Fatalf("Error starting rec: %v", err)
	}

	var (
		ring        sampleRing
		ampsOn      bool
		lastPlaying time.Time
	)

	setAmps := func(state bool) {
		if state {
			log.Printf("turning amps ON")
		} else {
			log.Printf("turning amps OFF")
		}
		ampsOn = state
		for _, amp := range amps {
			go setAmpState(amp, state)
		}
	}

	for {
		var sample int16
		err := binary.Read(out, binary.LittleEndian, &sample)
		if err != nil {
			log.Fatalf("error reading next sample: %v")
		}
		ring.Add(sample)
		if ring.i != 0 {
			continue
		}
		v := ring.Variance()
		audioPlaying := v > quietVarianceThreshold

		log.Printf("variance = %v; playing = %v", v, audioPlaying)
		if audioPlaying {
			lastPlaying = time.Now()
			if !ampsOn {
				setAmps(true)
			}
		} else if ampsOn {
			quietTime := time.Now().Sub(lastPlaying)
			if quietTime > time.Duration(*idleSec)*time.Second {
				setAmps(false)
			}
		}
	}
}
