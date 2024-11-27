package scheduler

// Result represents the result of a task execution.
// TODO: interfacify this too?
type Result struct {
	Data    map[string]interface{}
	Error   error
	Success bool
}

// Task is an interface for tasks that can be executed.
// TODO: consider adding a context.Context parameter to Execute, to handle timeouts and cancellation (can also be forcefully added in the worker)
type Task interface {
	Execute() Result
}
