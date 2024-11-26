package scheduler

import (
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// TODO: scaleup and scaledown functions for dynamic worker pool resizing
// TODO: tests for worker pool

// WorkerPool manages a pool of workers that execute tasks.
type WorkerPool struct {
	workerCount   int          // Total number of workers in the pool
	activeWorkers atomic.Int32 // Number of active workers

	// TODO: do a lookover for channel directions
	resultChan chan<- Result // Send-only channel for results
	stopChan   chan struct{} // Channel to signal stopping the worker pool
	taskChan   <-chan Task   // Receive-only channel for tasks

	wg sync.WaitGroup
}

// worker executes tasks from the task channel.
func (wp *WorkerPool) worker(id int) {
	wp.wg.Add(1)
	defer wp.wg.Done()

	for {
		select {
		case task, ok := <-wp.taskChan:
			if !ok {
				log.Debug().Msgf("Worker %d: task channel closed, exiting", id)
				return
			}
			log.Debug().Msgf("Worker %d executing task", id)
			wp.activeWorkers.Add(1) // Increment active workers
			// TODO: consider processing the task in a separate goroutine, e.g. async task execution within a single worker
			// TODO cont.: this would allow the worker to continue processing tasks while waiting for the result
			result := task.Execute()
			if result.Error != nil {
				// No retry policy is implemented, we just log the error for now
				// TODO: consider leaving all error handling to the caller
				log.Error().Err(result.Error).Msgf("Worker %d: task execution failed", id)
			}
			wp.resultChan <- result
			wp.activeWorkers.Add(-1) // Decrement active workers
			log.Debug().Msgf("Worker %d: finished task", id)
		case <-wp.stopChan:
			log.Debug().Msgf("Worker %d: received stop signal, exiting", id)
			return
		}
	}
}

// ActiveWorkers returns the number of active workers.
func (wp *WorkerPool) ActiveWorkers() int32 {
	return wp.activeWorkers.Load()
}

// Start starts the worker pool, creating workers according to wp.WorkerCount.
func (wp *WorkerPool) Start() {
	log.Info().Msgf("Starting worker pool with %d workers", wp.workerCount)
	for i := 0; i < wp.workerCount; i++ {
		go wp.worker(i)
	}
}

// Stop signals the worker pool to stop processing tasks and exit.
func (wp *WorkerPool) Stop() {
	log.Debug().Msg("Attempting worker pool stop")
	close(wp.stopChan) // Signal workers to stop
	log.Debug().Msg("Waiting for workers to finish")
	wp.wg.Wait() // Wait for all workers to finish
	close(wp.resultChan)
}

// NewWorkerPool creates a worker pool.
func NewWorkerPool(resultChannel chan Result, taskChannel chan Task, workerCount int) *WorkerPool {
	pool := &WorkerPool{
		resultChan:  resultChannel,
		stopChan:    make(chan struct{}),
		taskChan:    taskChannel,
		workerCount: workerCount,
	}
	return pool
}
