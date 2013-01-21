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
	"sync"
	"time"

	"code.google.com/p/go-avr/avr"
)

// Flags
var (
	ampAddrs = flag.String("amps", "", "Comma-separated list of ip:port of Denon amps")
	idle     = flag.Duration("idle", 5*time.Minute, "length of silence before turning off amps")
	alsaDev  = flag.String("alsadev", "", "If non-empty, arecord(1) is used instead of rec(1) with this ALSA device name. e.g. plughw:CARD=Audio,DEV=0 (see arecord -L)")
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

var (
	mu       sync.Mutex
	ampState = make(map[*avr.Amp]bool)
)

func getAmpState(amp *avr.Amp) (on bool, ok bool) {
	mu.Lock()
	defer mu.Unlock()
	on, ok = ampState[amp]
	return
}

func setAmpState(amp *avr.Amp, state bool) {
	if cur, ok := getAmpState(amp); ok && cur == state {
		return
	}

	cmds := []string{"ZMOFF", "PWSTANDBY"}
	if state {
		cmds = []string{"ZMON", "PWON"}
	}
	for _, cmd := range cmds {
		log.Printf("Sending command %q", cmd)
		err := amp.SendCommand(cmd)
		if err != nil {
			log.Printf("Sending command %q failed: %v", cmd, err)
			return
		}
	}

	log.Printf("Amp %s successfully set to state %v", amp.Addr(), state)
	mu.Lock()
	defer mu.Unlock()
	ampState[amp] = state
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
	if *alsaDev != "" {
		cmd = exec.Command("arecord",
			"-D", *alsaDev,
			"-f", "S16_LE",
			"-t", "raw")
	}
	out, _ := cmd.StdoutPipe()
	err := cmd.Start()
	if err != nil {
		log.Fatalf("Error starting rec: %v", err)
	}

	var (
		ring        sampleRing
		lastPlaying time.Time
	)

	setAmps := func(state bool) {
		allGood := true
		for _, amp := range amps {
			if cur, known := getAmpState(amp); !known || cur != state {
				allGood = false
				break
			}
		}
		if allGood {
			// All amps in the correct state; no need to log spam.
			return
		}
		if state {
			log.Printf("turning amps ON")
		} else {
			log.Printf("turning amps OFF")
		}
		for _, amp := range amps {
			go setAmpState(amp, state)
		}
	}

	for {
		var sample int16
		err := binary.Read(out, binary.LittleEndian, &sample)
		if err != nil {
			log.Fatalf("error reading next sample: %v", err)
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
			setAmps(true)
		} else if time.Since(lastPlaying) > *idle {
			setAmps(false)
		}
	}
}
