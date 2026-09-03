package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"harness/internal/config"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
	"harness/internal/session"
	"harness/internal/ui"
)

func fail(w io.Writer, code int, format string, args ...any) int {
	fmt.Fprintf(w, "harness: "+format+"\n", args...)
	return code
}

func startupInterruptedOrCanceled(interrupted func() bool, err error) bool {
	return interrupted() || errors.Is(err, context.Canceled)
}

func modelProxyFailure(stderr io.Writer, interrupted func() bool, err error) int {
	if startupInterruptedOrCanceled(interrupted, err) {
		return ui.ExitInterrupt
	}
	return fail(stderr, ui.ExitRuntime, "model proxy: %v", err)
}

func informationalModelCatalog(ctx context.Context, stderr io.Writer, proxyClient *modelclient.Client, interrupted func() bool) (protocol.Catalog, int) {
	catalog, err := checkModelProxy(ctx, proxyClient)
	if err != nil {
		return protocol.Catalog{}, modelProxyFailure(stderr, interrupted, err)
	}
	if interrupted() {
		return protocol.Catalog{}, ui.ExitInterrupt
	}
	return catalog, ui.ExitOK
}

func runRootInformational(ctx context.Context, stdout, stderr io.Writer, cfg config.Config, opts config.RunOptions, proxyClient *modelclient.Client, interrupted func() bool) (int, bool) {
	if !opts.ShowAgents && !opts.ShowModels && !opts.CheckModelProxy {
		return ui.ExitOK, false
	}
	if opts.ShowAgents || opts.ShowModels {
		var agents *agentsListOutput
		if opts.ShowAgents {
			var err error
			agents, err = buildAgentsListOutput(cfg)
			if err != nil {
				return fail(stderr, ui.ExitUsage, "agents: %v", err), true
			}
		}
		var models *modelsListOutput
		if opts.ShowModels {
			catalog, code := informationalModelCatalog(ctx, stderr, proxyClient, interrupted)
			if code != ui.ExitOK {
				return code, true
			}
			models = buildModelsListOutput(catalog)
		}
		if opts.OutputFormat == "json" {
			out := infoOutput{Version: 1}
			if agents != nil {
				out.DefaultAgent = agents.DefaultAgent
				out.SelectedAgent = agents.SelectedAgent
				out.Agents = agents.Agents
			}
			if models != nil {
				out.ProviderCount = models.ProviderCount
				out.ModelCount = models.ModelCount
				out.Models = sortedModelListEntries(models.Models)
			}
			if err := writeInformationalJSON(stdout, out); err != nil {
				return fail(stderr, ui.ExitRuntime, "info: %v", err), true
			}
			return ui.ExitOK, true
		}
		if agents != nil {
			fmt.Fprint(stdout, formatAgentsListText(*agents))
			if models != nil {
				fmt.Fprintln(stdout)
			}
		}
		if models != nil {
			fmt.Fprint(stdout, formatModelsListText(*models))
		}
		return ui.ExitOK, true
	}

	catalog, code := informationalModelCatalog(ctx, stderr, proxyClient, interrupted)
	if code != ui.ExitOK {
		return code, true
	}
	if opts.OutputFormat == "json" {
		out := infoOutput{
			Version:       1,
			ModelProxyURL: proxyClient.URL(),
			ProviderCount: 0,
			ModelCount:    catalogModelCount(catalog),
		}
		if err := writeInformationalJSON(stdout, out); err != nil {
			return fail(stderr, ui.ExitRuntime, "model proxy: %v", err), true
		}
	} else {
		fmt.Fprintf(stdout, "model proxy ok: %s (%d targets)\n", proxyClient.URL(), catalogModelCount(catalog))
	}
	return ui.ExitOK, true
}

type rootSessionLocks struct {
	active  *session.Lock
	pending *session.Lock
	retired []*session.Lock
}

func (locks *rootSessionLocks) close() {
	if locks.pending != nil {
		_ = locks.pending.Close()
	}
	if locks.active != nil {
		_ = locks.active.Close()
	}
	for _, lock := range locks.retired {
		_ = lock.Close()
	}
}

func (locks *rootSessionLocks) switchTo(path string) error {
	next, err := session.AcquireLock(path)
	if err != nil {
		return err
	}
	previous := locks.active
	locks.active = next
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

func (locks *rootSessionLocks) prepareChange(path string) error {
	if locks.pending != nil {
		return errors.New("session: another path change is pending")
	}
	next, err := session.AcquireLock(path)
	if err != nil {
		return err
	}
	locks.pending = next
	return nil
}

func (locks *rootSessionLocks) commitChange() {
	if locks.pending == nil {
		return
	}
	previous := locks.active
	locks.active = locks.pending
	locks.pending = nil
	if previous != nil {
		// Canceled background children can finish their final checkpoint after a
		// path rotation returns. Retain ownership of old roots until process exit
		// so they cannot race a resume of the prior session.
		locks.retired = append(locks.retired, previous)
	}
}

type resumeClone struct {
	Created time.Time
	From    string
	To      string
}

func cloneSessionForResume(resumed *session.Session, now func() time.Time) (resumeClone, error) {
	clone := resumeClone{Created: now(), From: resumed.Tree.ActiveLeaf}
	cloneCWD := resumed.CWD
	if current, err := os.Getwd(); err == nil {
		cloneCWD = current
	}
	cloneTree, err := resumed.Tree.Extract(clone.From, clone.Created, cloneCWD)
	if err != nil {
		return resumeClone{}, err
	}
	clone.To, err = cloneTree.AppendBranch(clone.From, clone.From, clone.From, "", "")
	if err != nil {
		return resumeClone{}, err
	}
	cloneMessages, err := cloneTree.BuildContext()
	if err != nil {
		return resumeClone{}, err
	}
	resumed.Tree = cloneTree
	resumed.Messages = cloneMessages
	resumed.Created = clone.Created
	resumed.Updated = clone.Created
	resumed.Prompt = 0
	resumed.ProxySessionID = ""
	resumed.CacheAffinityID = ""
	resumed.ResponseState = nil
	resumed.Usage = session.UsageTotals{}
	resumed.UsageByModel = nil
	resetTrajectoryForResumeClone(resumed)
	return clone, nil
}
