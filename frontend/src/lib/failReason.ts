// No imports: scripts/fail-reason.test.mjs loads this file in plain node.

/**
 * The reason a failed-job toast shows: our own text (app.failReason.<code>)
 * when the code has one, else the first line of the job's error. agent_failed
 * has no text on purpose: «the agent failed» repeats the toast title, the
 * CLI's own error says why.
 */
export function jobFailReason(code: string, error: string, text: (key: string) => string | undefined): string {
  return (code && text('app.failReason.' + code)) || error.split('\n')[0].trim()
}
