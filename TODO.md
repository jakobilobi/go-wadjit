# todo

## v0.x

- Clear in-code todo:s
- Implement Wadjit function to get info on held watchers, perhaps a []string with ID:s
- Ability attach appData to request tasks, to track responses more easily (shared response channel)

## various ideas

- Create a new type of `Watcher`, to watch WS subscriptions
  - WS subscriptions do not fit into the current `Watcher` model, which is based on polling
- Add metrics for stats like `Watcher` timing accuracy, and `Watcher` error rates
- Add configuration options, e.g. for buffer sizes, timeouts, metrics on/off etc.
- WS reconnect strategy evaluation
  - Change for an active reconnect, with exponential backoff, would also include keep-alive measures
  - Introduce a pre-execute check that the connection is alive and ready to be used
- Make WS use read and write deadlines to avoid blocking
