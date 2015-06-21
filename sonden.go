// Copyright 2011 Google Inc.
// See LICENSE file.
//
// Home automation & power saving daemon.
//
// Listens to audio input and turns on the amplifier when there's
// music and turns it off to save power when things are silent for
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
	"sync"
	"time"

	"code.google.com/p/go-avr/avr"
)

// Flags
var (
	ampAddr   = flag.String("amp", "", "Amplifier's address (ip:port)")
	idle      = flag.Duration("idle", 5*time.Minute, "How much silence before turning off amp")
	playing   = flag.Duration("playing", 7*time.Second, "How much music before turning on amp")
	alsaDev   = flag.String("alsadev", "", "If non-empty, arecord(1) is used instead of rec(1) with this ALSA device name. e.g. plughw:CARD=Audio,DEV=0 (see arecord -L)")
	threshold = flag.Float64("threshold", 0, "Optional manual sound cut-off threshold to use")
	dryRun    = flag.Bool("dry_run", true, "Whether to actually send commands to the amplifier.")
	debug     = flag.Bool("debug", true, "Whether to spam the output with log messages.")
)

const (
	quietVarianceThreshold     = 1000 // typically ~2. occasionally as high as 16.
	alsaQuietVarianceThreshold = 2000 // why different than previous line? dunno. wrong sample params?
	sampleHz                   = 8 << 10
	ringSize                   = sampleHz // 2 seconds of audio
)

var (
	mu              sync.Mutex
	currentAmpState bool
)

type sampleRing struct {
	i       int
	samples [ringSize]int16
	sum     int
}

type varianceWindow struct {
	size              int
	i                 int
	lastConseqPlaying int
	totalNotPlaying   int
}

func (vw *varianceWindow) Add(variance float64) {
	vw.i++
	if vw.i == vw.size {
		vw.i = 0
	}

	isNowPlaying := variance > *threshold
	if isNowPlaying {
		vw.lastConseqPlaying = min(vw.size, vw.lastConseqPlaying+1)
		vw.totalNotPlaying = max(0, vw.totalNotPlaying-1)
	} else {
		vw.lastConseqPlaying = 0
		vw.totalNotPlaying = min(vw.size, vw.totalNotPlaying+1)
	}
	if *debug {
		if isNowPlaying {
			log.Print("\t\tAnd we're playing, baby!")
		} else {
			log.Print("\t\tAnd we're not playing tonight, baby, I'm sorry")
		}
		log.Printf("variance = %.2f, threshold = %d", variance, threshold)
		log.Printf("vw.lastConseqPlaying = %d, vw.totalNotPlaying = %d, GoodToTurnOn() = %v, GoodToTurnOff() = %v, size = %d",
			vw.lastConseqPlaying, vw.totalNotPlaying, vw.GoodToTurnOn(), vw.GoodToTurnOff(), vw.size)
	}
}

// good to turn on == N variances in a row say so
func (vw *varianceWindow) GoodToTurnOn() bool {
	return vw.lastConseqPlaying >= int(playing.Seconds())
}

// good to turn off == most of variances say so
func (vw *varianceWindow) GoodToTurnOff() bool {
	return vw.totalNotPlaying >= int(idle.Seconds())
}

func (r *sampleRing) Add(sample int16) {
	r.sum -= int(r.samples[r.i])
	r.sum += int(sample)
	r.samples[r.i] = sample
	r.i++
	if r.i == len(r.samples) {
		r.i = 0
	}
}

func (r *sampleRing) Variance() float64 {
	mean := float64(r.sum) / float64(len(r.samples))
	v := 0.0
	for _, sample := range r.samples {
		v += math.Pow(math.Abs(float64(mean)-float64(sample)), 2)
	}
	v /= float64(len(r.samples))
	return v
}

func turnOnOrOff(state bool) {
	mu.Lock()
	defer mu.Unlock()
	if currentAmpState == state {
		return
	}
	currentAmpState = state

	log.Printf("Connecting to %s ...", *ampAddr)
	amp := avr.New(*ampAddr)
	if err := amp.Ping(); err != nil {
		log.Printf("Error connecting to amp at %s: %v. Let's try later.", *ampAddr, err)
		currentAmpState = !state
		return
	}
	log.Printf("Connected to AVR.")

	// TODO(radkat): Increase/decrease volume as well.
	cmds := []string{"PWSTANDBY"}
	if state {
		cmds = []string{"PWON"}
	}

	for _, cmd := range cmds {
		log.Printf("Sending command to %s: %q", amp.Addr(), cmd)
		if *dryRun {
			log.Printf("I could've executed this but I won't: %v", cmd)
		} else {
			err := amp.SendCommand(cmd)
			if err != nil {
				log.Printf("Sending command %q to %s failed: %v", cmd, amp.Addr(), err)
				currentAmpState = !state
				return
			}
		}
	}

	log.Printf("Amp %s successfully set to state %v", amp.Addr(), state)
	time.Sleep(1 * time.Second) // otherwise, will close before the command gets executed
	amp.Close()
}

func main() {
	flag.Parse()

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
	if *threshold == 0 {
		if *alsaDev != "" {
			*threshold = alsaQuietVarianceThreshold
		} else {
			*threshold = quietVarianceThreshold
		}
	}
	out, _ := cmd.StdoutPipe()
	err := cmd.Start()
	if err != nil {
		log.Fatalf("Error starting rec: %v", err)
	}

	var (
		ring sampleRing
		vw   varianceWindow
	)
	vw.size = int(idle.Seconds() + playing.Seconds())

	var sample int16
	for {
		err := binary.Read(out, binary.LittleEndian, &sample)
		if err != nil {
			log.Fatalf("error reading next sample: %v", err)
		}
		ring.Add(sample)

		// Waiting for the ring to (re-)fill
		if ring.i != 0 {
			continue
		}

		vw.Add(ring.Variance())

		if vw.GoodToTurnOn() {
			turnOnOrOff(true)
		}

		if vw.GoodToTurnOff() {
			turnOnOrOff(false)
		}
	}
}

// min(a,b) = a iff a-b <= 0
func min(a, b int) int {
	return compare(a, b, func(d int) bool {
		return d <= 0
	})
}

// max(a,b) = a iff a-b >= 0
func max(a, b int) int {
	return compare(a, b, func(d int) bool {
		return d >= 0
	})
}

func compare(a, b int, fn func(int) bool) int {
	diff := a - b
	if fn(diff) {
		return a
	} else {
		return b
	}
}
