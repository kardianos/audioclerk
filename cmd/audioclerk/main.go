package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/audioclerk"
	"github.com/kardianos/task"
)

func main() {
	err := task.Start(context.Background(), time.Second*2, run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cmd := &task.Command{
		Name:  os.Args[0],
		Usage: `Transcribe a single file or watch a folder for any applicable files to translate.`,
		Flags: []*task.Flag{
			{Name: "model", Usage: `Whisper ggml model.`, Default: filepath.Join(homeDir, ".cache/whisper.cpp/ggml-model.bin")},
		},
		Commands: []*task.Command{{
			Name:  "transcribe",
			Usage: `Transcribe a single file, provide the input file.`,
			Flags: []*task.Flag{
				{Name: "i", Usage: `Input filename`, Default: ""},
				{Name: "o", Usage: `Output filename, if not present, will use the input filename with a suffix appended.`, Default: ""},
			},
			Action: task.ActionFunc(func(ctx context.Context, st *task.State, sc task.Script) error {
				return audioclerk.Transcribe(ctx, st.Get("model").(string), st.Get("i").(string), st.Get("o").(string))
			}),
		}, {
			Name:  "watch",
			Usage: `Watch a folder to translate.`,
			Flags: []*task.Flag{
				{Name: "f", Usage: `Folder name to watch, use ";" to separate out multiple paths.`, Default: ""},
			},
			Action: task.ActionFunc(func(ctx context.Context, st *task.State, sc task.Script) error {
				f := st.Get("f").(string)
				fl := strings.Split(f, ";")
				return audioclerk.Watch(ctx, st.Get("model").(string), fl)
			}),
		}},
	}
	a := cmd.Exec(os.Args[1:])
	st := task.DefaultState()
	return task.Run(ctx, st, a)
}
