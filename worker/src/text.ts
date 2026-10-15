// Agent-written text (goals, reasons, paths, names) is shown later in
// terminals and browsers. Control characters there can rewrite a reviewer's
// screen (ANSI escapes, carriage returns) or reorder what they read (bidi
// overrides), so the Worker refuses them at the door. Commit messages and
// author names are never stored (see events.ts), so they need no check here.

// C0 controls except tab, newline and carriage return; DEL; C1 controls;
// bidi embeddings, overrides and isolates.
const CONTROL_MULTILINE = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/;
const CONTROL_SINGLE_LINE = /[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/;

/** True when s holds a character no plain-text field should carry. multiline allows \t, \n and \r. */
export function hasControl(s: string, multiline = true): boolean {
  return (multiline ? CONTROL_MULTILINE : CONTROL_SINGLE_LINE).test(s);
}
