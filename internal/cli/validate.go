package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/shafi-/loop/internal/config"
)

func newValidateCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "validate <file>...",
		Short: "Validate pipeline, room, or persona YAML files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			failed := false
			for _, path := range args {
				if err := validateOne(path, kind); err != nil {
					failed = true
					fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s\n  %v\n", path, err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", path)
			}
			if failed {
				return errors.New("validation failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "type", "auto", "document kind: auto, pipeline, room, or persona")
	return cmd
}

func validateOne(path, kind string) error {
	// Sniff the top-level document to decide which schema applies; --type overrides.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if len(node.Content) == 0 {
		return fmt.Errorf("file is empty")
	}

	effKind := config.DocumentKind(kind)
	if effKind == "" || effKind == "auto" {
		detected, ok := config.IdentifyKind(node.Content[0])
		if !ok {
			return fmt.Errorf("cannot tell what this is (no \"stages\", \"agents\", or \"system\" key); pass --type")
		}
		effKind = detected
	}

	switch effKind {
	case config.KindPipeline:
		_, err = config.LoadPipeline(path)
	case config.KindRoom:
		_, err = config.LoadRoom(path)
	case config.KindPersona:
		_, err = config.LoadPersona(path)
	default:
		err = fmt.Errorf("unknown --type %q (want auto, pipeline, room, or persona)", kind)
	}
	return err
}
