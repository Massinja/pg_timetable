package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_timetable/internal/log"
	"github.com/cybertec-postgresql/pg_timetable/internal/pgengine"
	pgx "github.com/jackc/pgx/v4"
)

// Chain structure used to represent tasks chains
type Chain struct {
	ChainID            int    `db:"chain_id"`
	TaskID             int    `db:"task_id"`
	ChainName          string `db:"chain_name"`
	SelfDestruct       bool   `db:"self_destruct"`
	ExclusiveExecution bool   `db:"exclusive_execution"`
	MaxInstances       int    `db:"max_instances"`
}

func (chain Chain) String() string {
	data, _ := json.Marshal(chain)
	return string(data)
}

// Lock locks the chain in exclusive or non-exclusive mode
func (sch *Scheduler) Lock(exclusiveExecution bool) {
	if exclusiveExecution {
		sch.exclusiveMutex.Lock()
	} else {
		sch.exclusiveMutex.RLock()
	}
}

// Unlock releases the lock after the chain execution
func (sch *Scheduler) Unlock(exclusiveExecution bool) {
	if exclusiveExecution {
		sch.exclusiveMutex.Unlock()
	} else {
		sch.exclusiveMutex.RUnlock()
	}
}

func (sch *Scheduler) retrieveAsyncChainsAndRun(ctx context.Context) {
	for {
		chainSignal := sch.pgengine.WaitForChainSignal(ctx)
		if chainSignal.ConfigID == 0 {
			return
		}
		switch chainSignal.Command {
		case "START":
			var headChain Chain
			err := sch.pgengine.SelectChain(ctx, &headChain, chainSignal.ConfigID)
			if err != nil {
				sch.l.WithError(err).Error("Could not query pending tasks")
			} else {
				sch.l.WithField("chain", headChain.ChainID).
					Debug("Putting head chain to the execution channel")
				sch.chainsChan <- headChain
			}
		case "STOP":
			if cancel, ok := sch.activeChains[chainSignal.ConfigID]; ok {
				cancel()
			}
		}
	}
}

func (sch *Scheduler) retrieveChainsAndRun(ctx context.Context, reboot bool) {
	var err error
	msg := "Retrieve scheduled chains to run"
	if reboot {
		msg = msg + " @reboot"
	}
	headChains := []Chain{}
	if reboot {
		err = sch.pgengine.SelectRebootChains(ctx, &headChains)
	} else {
		err = sch.pgengine.SelectChains(ctx, &headChains)
	}
	if err != nil {
		sch.l.WithError(err).Error("Could not query pending tasks")
		return
	}
	headChainsCount := len(headChains)
	sch.l.WithField("count", headChainsCount).Info(msg)
	// now we can loop through so chains
	for _, headChain := range headChains {
		// if the number of chains pulled for execution is high, try to spread execution to avoid spikes
		if headChainsCount > sch.Config().Resource.CronWorkers*refetchTimeout {
			time.Sleep(time.Duration(refetchTimeout*1000/headChainsCount) * time.Millisecond)
		}
		sch.l.WithField("chain", headChain.ChainID).
			Debug("Putting head chain to the execution channel")
		sch.chainsChan <- headChain
	}
}

func (sch *Scheduler) addActiveChain(id int, cancel context.CancelFunc) {
	sch.activeChainMutex.Lock()
	sch.activeChains[id] = cancel
	sch.activeChainMutex.Unlock()
}

func (sch *Scheduler) deleteActiveChain(id int) {
	sch.activeChainMutex.Lock()
	delete(sch.activeChains, id)
	sch.activeChainMutex.Unlock()
}

func (sch *Scheduler) chainWorker(ctx context.Context, chains <-chan Chain) {
	for {
		select {
		case <-ctx.Done(): //check context with high priority
			return
		default:
			select {
			case chain := <-chains:
				chainL := sch.l.WithField("chain", chain.ChainID)
				chainContext := log.WithLogger(ctx, chainL)
				chainL.Info("Starting chain")
				if !sch.pgengine.CanProceedChainExecution(chainContext, chain.ChainID, chain.MaxInstances) {
					chainL.Debug("Cannot proceed. Sleeping")
					continue
				}
				sch.Lock(chain.ExclusiveExecution)
				chainContext, cancel := context.WithCancel(chainContext)
				sch.addActiveChain(chain.TaskID, cancel)
				sch.executeChain(chainContext, chain.ChainID, chain.TaskID)
				if chain.SelfDestruct {
					sch.pgengine.DeleteChainConfig(chainContext, chain.ChainID)
				}
				sch.deleteActiveChain(chain.TaskID)
				cancel()
				sch.Unlock(chain.ExclusiveExecution)
			case <-ctx.Done():
				return
			}

		}
	}
}

/* execute a chain of tasks */
func (sch *Scheduler) executeChain(ctx context.Context, chainID int, taskID int) {
	var ChainElements []pgengine.ChainElement
	var bctx context.Context
	chainL := sch.l.WithField("chain", chainID)

	tx, err := sch.pgengine.StartTransaction(ctx)
	if err != nil {
		chainL.WithError(err).Error("Cannot start transaction")
		return
	}

	if !sch.pgengine.GetChainElements(ctx, tx, &ChainElements, taskID) {
		sch.pgengine.MustRollbackTransaction(ctx, tx)
		return
	}

	runStatusID := sch.pgengine.InsertChainRunStatus(ctx, chainID, taskID)

	/* now we can loop through every element of the task chain */
	for _, chainElem := range ChainElements {
		chainElem.ChainID = chainID
		l := chainL.WithField("task", chainElem.CommandID)
		l.Info("Starting task")
		ctx = log.WithLogger(ctx, l)
		sch.pgengine.UpdateChainRunStatus(ctx, &chainElem, runStatusID, "STARTED")
		retCode := sch.executeСhainElement(ctx, tx, &chainElem)

		// we use background context here because current one (ctx) might be cancelled
		bctx = log.WithLogger(context.Background(), l)
		if retCode != 0 && !chainElem.IgnoreError {
			chainL.Error("Chain failed")
			sch.pgengine.UpdateChainRunStatus(bctx, &chainElem, runStatusID, "CHAIN_FAILED")
			sch.pgengine.MustRollbackTransaction(bctx, tx)
			return
		}
		sch.pgengine.UpdateChainRunStatus(bctx, &chainElem, runStatusID, "CHAIN_DONE")
	}
	chainL.Info("Chain executed successfully")
	bctx = log.WithLogger(context.Background(), chainL)
	sch.pgengine.UpdateChainRunStatus(bctx,
		&pgengine.ChainElement{
			TaskID:  taskID,
			ChainID: chainID}, runStatusID, "CHAIN_DONE")
	sch.pgengine.MustCommitTransaction(bctx, tx)
}

func (sch *Scheduler) executeСhainElement(ctx context.Context, tx pgx.Tx, chainElem *pgengine.ChainElement) int {
	var paramValues []string
	var err error
	var out string
	var retCode int
	l := log.GetLogger(ctx)
	if !sch.pgengine.GetChainParamValues(ctx, tx, &paramValues, chainElem) {
		return -1
	}

	chainElem.StartedAt = time.Now()
	switch chainElem.Kind {
	case "SQL":
		out, err = sch.pgengine.ExecuteSQLTask(ctx, tx, chainElem, paramValues)
	case "PROGRAM":
		if sch.pgengine.NoProgramTasks {
			l.Info("Program task execution skipped")
			return -1
		}
		retCode, out, err = sch.ExecuteProgramCommand(ctx, chainElem.Script, paramValues)
	case "BUILTIN":
		out, err = sch.executeTask(ctx, chainElem.CommandName, paramValues)
	}
	chainElem.Duration = time.Since(chainElem.StartedAt).Microseconds()

	if err != nil {
		if retCode == 0 {
			retCode = -1
		}
		out = strings.Join([]string{out, err.Error()}, "\n")
		l.WithError(err).Error("Task execution failed")
	} else {
		l.Info("Task executed successfully")
	}
	sch.pgengine.LogChainElementExecution(context.Background(), chainElem, retCode, out)
	return 0
}
