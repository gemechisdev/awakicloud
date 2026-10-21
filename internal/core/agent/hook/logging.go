package hook

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func LoggingBeforeIteration() BeforeIterationFunc {
	return func(_ context.Context, ic *IterationContext) {
		log.Info().
			Int("iteration", ic.Iteration).
			Str("model", ic.Model).
			Int("messages", len(ic.Messages)).
			Msg("iteration start")

		if log.Logger.GetLevel() <= zerolog.TraceLevel {
			log.Trace().Msg(formatRequestDump(ic))
		}
	}
}

func LoggingAfterIteration(inputPricePerM, outputPricePerM float64) AfterIterationFunc {
	return func(_ context.Context, ic *IterationContext, result *IterationResult) {
		resp := result.Response
		cost := CalcCost(inputPricePerM, outputPricePerM, resp.InputTokens, resp.OutputTokens)
		evt := log.Info().
			Int("iteration", ic.Iteration).
			Str("elapsed", formatDuration(result.LLMDuration)).
			Int("in_tokens", resp.InputTokens).
			Int("out_tokens", resp.OutputTokens).
			Str("cost", fmt.Sprintf("$%.4f", cost)).
			Str("stop", resp.StopReason)

		if len(result.ToolCalls) > 0 {
			names := make([]string, 0, len(result.ToolCalls))
			for _, tc := range result.ToolCalls {
				names = append(names, tc.Name)
			}
			evt.Str("tools", strings.Join(names, ", "))
		}

		evt.Msg("iteration done")

		if log.Logger.GetLevel() <= zerolog.TraceLevel {
			log.Trace().Msg(formatResponseDump(ic, result))
		}
	}
}
