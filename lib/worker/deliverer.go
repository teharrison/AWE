package worker

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MG-RAST/AWE/lib/cache"
	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"

	shock "github.com/MG-RAST/go-shock-client"
)

func deliverer(control chan int) {
	fmt.Printf("deliverer launched, client=%s\n", core.Self.ID)
	defer fmt.Printf("deliverer exiting...\n")
	for {
		err := deliverer_run(control)
		if err != nil {
			logger.Error("(deliverer) deliverer_run returned: %s", err.Error())
		}
	}
	//control <- ID_DELIVERER //we are ending
}

func deliverer_run(control chan int) (err error) { // TODO return all errors

	logger.Debug(3, "deliverer_run")

	// this makes sure new work is only requested when deliverer is done
	defer func() { <-chanPermit }()
	workunit := <-fromProcessor

	if Client_mode == "offline" {
		return
	}

	work_id := workunit.Workunit_Unique_Identifier

	var work_str string
	work_str, err = work_id.String()
	if err != nil {
		return
	}
	logger.Debug(3, "(deliverer_run) work_id: %s", work_str)
	work_state, ok, err := workmap.Get(work_id)
	if err != nil {
		logger.Error("error: %s", err.Error())
		return
	}
	if !ok {
		logger.Error("(deliverer) work id %s not found", work_str)
		return
	}

	if work_state == ID_DISCARDED {
		workunit.SetState(core.WORK_STAT_DISCARDED, "workmap indicated discarded")
		logger.Event(event.WORK_DISCARD, "workid="+work_str)
	} else {
		workmap.Set(work_id, ID_DELIVERER, "deliverer")
		perfstat := workunit.WorkPerf

		// post-process for works computed successfully: push output data to Shock
		move_start := time.Now().UnixNano()
		logger.Debug(3, "(deliverer_run) work.State: %s", workunit.State)
		if workunit.State == core.WORK_STAT_COMPUTED {

			shock_client := &shock.ShockClient{Host: workunit.ShockHost, Token: workunit.Info.DataToken, Debug: false}

			data_moved, err := cache.UploadOutputData(workunit, shock_client, nil)
			if err != nil {
				workunit.SetState(core.WORK_STAT_ERROR, "UploadOutputData failed")
				logger.Error("(deliverer_run) UploadOutputData returns workid=" + work_str + ", err=" + err.Error())
				workunit.Notes = append(workunit.Notes, "[deliverer#UploadOutputData]"+err.Error())
			} else {
				workunit.SetState(core.WORK_STAT_DONE, "")
				perfstat.OutFileSize = data_moved
			}
		}
		move_end := time.Now().UnixNano()
		perfstat.DataOut = float64(move_end-move_start) / 1e9
		perfstat.Deliver = int64(move_end / 1e9)
		perfstat.ClientResp = perfstat.Deliver - perfstat.Checkout
		perfstat.ClientId = core.Self.ID

		// notify server the final process results; send perflog, stdout, and stderr if needed
		// detect e.ClientNotFound
		do_retry := true
		retry_count := 0
		for do_retry {
			response, err := core.NotifyWorkunitProcessedWithLogs(workunit, perfstat, conf.PRINT_APP_MSG)
			if err != nil {
				logger.Error("(deliverer_run) workid=%s NotifyWorkunitProcessedWithLogs returned: %s", work_str, err.Error())
				workunit.Notes = append(workunit.Notes, "[deliverer]"+err.Error())
				error_message := strings.Join(response.Error, ",")
				if strings.Contains(error_message, e.ClientNotFound) { // TODO need better method than string search. Maybe a field awe_status.
					//mark this work in Current_work map as false, something needs to be done in the future
					//to clean this kind of work that has been proccessed but its result can't be sent to server!
					//core.Self.Current_work_false(work.Id) //server doesn't know this yet
					do_retry = false
				}
				// keep retry
			} else {

				if response.Status == http.StatusOK {
					// success, work delivered
					logger.Debug(1, "work delivered successfully")
					do_retry = false
				} else {
					error_message := strings.Join(response.Error, ",")
					logger.Error("(deliverer) response.Status not ok,  workid=%s, err=%s", work_str, error_message)
				}
			}

			if do_retry {
				time.Sleep(time.Second * 60)
				retry_count += 1
			} else {
				if retry_count > 100 { // TODO 100 ?
					break
				}
				break
			}
		}
	}

	work_path, err := workunit.Path()
	if err != nil {
		return
	}

	// now final status report sent to server, update some local info
	if workunit.State == core.WORK_STAT_DONE {
		logger.Event(event.WORK_DONE, "workid="+work_str)
		core.Self.IncrementTotalCompleted()
		if conf.AUTO_CLEAN_DIR && workunit.Cmd.Local == false {
			go removeDirLater(work_path, conf.CLIEN_DIR_DELAY_DONE)
		}
	} else {
		if workunit.State == core.WORK_STAT_DISCARDED {
			logger.Event(event.WORK_DISCARD, "workid="+work_str)
		} else {
			logger.Event(event.WORK_RETURN, "workid="+work_str)
		}
		core.Self.IncrementTotalFailed(true)
		if conf.AUTO_CLEAN_DIR && workunit.Cmd.Local == false {
			go removeDirLater(work_path, conf.CLIEN_DIR_DELAY_FAIL)
		}
	}

	// cleanup
	err = core.Self.CurrentWork.Delete(work_id, true)
	if err != nil {
		logger.Error("Could not remove work_id %s", work_str)
	}
	workmap.Delete(work_id)

	var empty bool
	empty, _ = core.Self.CurrentWork.IsEmpty(false)
	if empty {
		_ = core.Self.SetBusy(false, false)
	}
	return
}

func removeDirLater(path string, duration time.Duration) (err error) {
	time.Sleep(duration)
	return os.RemoveAll(path)
}
