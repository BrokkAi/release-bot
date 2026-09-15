package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	bot "github.com/BrokkAi/release-bot"
)

func historyCommand(cfg bot.Config, tag string, jsonOutput bool, out io.Writer) error {
	state, err := bot.ReadState(cfg)
	if err != nil {
		return err
	}
	receipts := state.VerifiedHistory()
	if tag != "" {
		selected := []bot.VerifiedReceipt{}
		for _, receipt := range receipts {
			if receipt.Result.Tag == tag {
				selected = append(selected, receipt)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("no verified receipt for tag %q", tag)
		}
		receipts = selected
	}
	const meaning = "Historical daemon verification succeeded; no live reverification. Result detail and plan check evidence are agent-supplied text. Private local diagnostics, not tamper-proof audit evidence."
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			Meaning  string                `json:"meaning"`
			Receipts []bot.VerifiedReceipt `json:"receipts"`
		}{meaning, receipts})
	}
	fmt.Fprintln(out, meaning)
	if len(receipts) == 0 {
		_, err = fmt.Fprintln(out, "No verified receipts recorded.")
		return err
	}
	for _, receipt := range receipts {
		at := "unknown verification time"
		if receipt.VerifiedAt != nil {
			at = receipt.VerifiedAt.UTC().Format(time.RFC3339Nano)
		}
		r := receipt.Result
		fmt.Fprintf(out, "%s  %s  %s  %s\n", at, r.Tag, r.Commit, r.URL)
		if tag != "" {
			fmt.Fprintf(out, "Agent-supplied detail: %s\nSaved destination plan (check evidence is agent-supplied):\n", r.Detail)
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(r.Plan); err != nil {
				return err
			}
		}
	}
	return nil
}
