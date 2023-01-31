package audioclerk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-audio/wav"
	"github.com/kardianos/whisper.cpp/tts"
)

type system struct {
	model tts.Model
}

func newSystem(ctx context.Context, modelPath string) (*system, error) {
	if ce := ctx.Err(); ce != nil {
		return nil, ce
	}
	model, err := tts.New(modelPath)
	if err != nil {
		return nil, fmt.Errorf("model load: %w", err)
	}
	if ce := ctx.Err(); ce != nil {
		model.Close()
		return nil, ce
	}

	return &system{
		model: model,
	}, nil
}

func (s *system) Close() error {
	if s == nil {
		return nil
	}
	if s.model == nil {
		return nil
	}
	return s.model.Close()
}

func (s *system) Transcribe(ctx context.Context, inputPath, outputPath string) error {
	var data []float32

	mc, err := s.model.NewContext()
	if err != nil {
		return err
	}

	// Speedups appears limited after 4 threads.
	threadCount := runtime.NumCPU()
	if threadCount > 4 {
		threadCount = 4
	}
	mc.SetThreads(uint(threadCount))

	err = mc.SetLanguage("en")
	if err != nil {
		return err
	}

	var tempFile string
	if true {
		// ffmpeg -loglevel -0 -y -i gb0.ogg -ar 16000 -ac 1 -c:a pcm_s16le gb0.wav
		outBuf := &bytes.Buffer{}
		errBuf := &bytes.Buffer{}

		// I'd like to just pipe from ffmpeg, but ffmpeg outputs differently if piped.
		f, err := os.CreateTemp("", "*.wav")
		if err != nil {
			return err
		}
		tempFile = f.Name()
		f.Close()

		cmd := exec.CommandContext(ctx, "ffmpeg",
			"-hide_banner",
			"-loglevel", "error",
			"-i", inputPath,
			"-ar", "16000",
			"-ac", "1",
			"-c:a",
			"pcm_s16le",
			"-y",
			"-f", "wav",
			tempFile,
		)
		cmd.Stdout = outBuf
		cmd.Stderr = errBuf
		err = cmd.Run()
		if err != nil {
			if errBuf.Len() > 0 {
				return fmt.Errorf("ffmpeg: %w\n%s", err, errBuf.Bytes())
			}
			return fmt.Errorf("ffmpeg: %w", err)
		}
	}
	if true {
		fh, err := os.Open(tempFile)
		if err != nil {
			return err
		}
		dec := wav.NewDecoder(fh)
		buf, err := dec.FullPCMBuffer()
		fh.Close()
		if err != nil {
			return err
		}

		frames := buf.NumFrames()
		if frames == 0 {
			return fmt.Errorf("no audio frames")
		}
		if dec.SampleRate != tts.SampleRate {
			return fmt.Errorf("unsupported sample rate: %d", dec.SampleRate)
		}
		if dec.NumChans != 1 {
			return fmt.Errorf("unsupported number of channels: %d", dec.NumChans)
		}

		data = buf.AsFloat32Buffer().Data
	}
	if len(tempFile) > 0 {
		os.Remove(tempFile)
	}

	if ce := ctx.Err(); ce != nil {
		return ce
	}

	err = mc.Process(data, nil)
	if err != nil {
		return err
	}

	if len(outputPath) == 0 {
		outputPath = inputPath + ".txt"
	}

	outf, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outf.Close()

	outBuf := bufio.NewWriter(outf)

	out := outBuf
	const color = false
	const onlyText = true

	for {
		if ce := ctx.Err(); ce != nil {
			return ce
		}
		segment, err := mc.NextSegment()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			outBuf.Flush()
			return err
		}
		fmt.Fprintf(out, "%10s: %s\n", segment.Start.Truncate(time.Second/10), segment.Text)
	}
	err = outBuf.Flush()
	if err != nil {
		return err
	}
	return outf.Close()
}

func Watch(ctx context.Context, modelPath string, dirList []string) error {
	for _, fn := range dirList {
		fi, err := os.Stat(fn)
		if err != nil {
			return fmt.Errorf("unable to watch, cannot access %q: %w", fn, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("path %q is not a directory", fn)
		}
	}

	s, err := newSystem(ctx, modelPath)
	if err != nil {
		return err
	}
	defer s.Close()

	const checkInterval = time.Second * 10
	var lastCheckTime time.Time
	enqueue := make(chan string, 50)
	queue := map[string]bool{}
	errorReport := make(chan error, 50)

	inTranscription := &atomic.Bool{}

	tick := time.NewTicker(time.Second * 10)
	defer tick.Stop()

	const suffix = ".txt"
	ext := map[string]bool{
		".mp3": true,
		".mp4": true,
		".ogg": true,
		".m4a": true,
		".wav": true,
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case fn := <-enqueue:
			queue[fn] = true
		case err := <-errorReport:
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		case now := <-tick.C:
			if inTranscription.Load() {
				continue
			}

			for fn := range queue {
				delete(queue, fn)

				go func(fn string) {
					if !inTranscription.CompareAndSwap(false, true) {
						return
					}
					defer inTranscription.Store(false)

					fnText := fn + suffix
					_, err := os.Stat(fnText)
					if err == nil {
						return
					}

					start := time.Now()
					fmt.Fprintf(os.Stdout, "START: %q...", fn)
					err = s.Transcribe(ctx, fn, fnText)
					dur := time.Now().Sub(start).Truncate(time.Millisecond)

					if err != nil {
						fmt.Fprintf(os.Stdout, "ERROR (%v)\n", dur)
						errorReport <- fmt.Errorf("Transcribe %q: %w", fn, err)
					}
					fmt.Fprintf(os.Stdout, "DONE (%v).\n", dur)
				}(fn)
				break
			}
			if now.Sub(lastCheckTime) < checkInterval {
				continue
			}
			lastCheckTime = now
			for _, dn := range dirList {
				list, err := readDir(dn, ext, suffix)
				if err != nil {
					errorReport <- fmt.Errorf("read dir %q: %w", dn, err)
				}
				for _, item := range list {
					queue[item] = true
				}
			}
		}
	}
}

func readDir(dn string, allowExt map[string]bool, suffix string) ([]string, error) {
	d, err := os.Open(dn)
	if err != nil {
		return nil, fmt.Errorf("dir open %w", err)
	}
	defer d.Close()

	var list []string
	for {
		dl, err := d.ReadDir(50)
		if err != nil {
			if err == io.EOF {
				return list, nil
			}
			return list, err
		}
		for _, fi := range dl {
			if fi.IsDir() {
				continue
			}
			fn := filepath.Join(dn, fi.Name())
			ext := strings.ToLower(filepath.Ext(fn))
			if !allowExt[ext] {
				continue
			}
			fnText := fn + suffix
			if _, err := os.Stat(fnText); err == nil {
				continue
			}
			list = append(list, fn)
		}
	}
}

func Transcribe(ctx context.Context, modelPath, inputPath, outputPath string) error {
	s, err := newSystem(ctx, modelPath)
	if err != nil {
		return err
	}
	defer s.Close()

	return s.Transcribe(ctx, inputPath, outputPath)
}
