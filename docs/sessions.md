# Sessions, the status bar and the terminal

`aicatraz up` (and the agent commands, in a terminal) run the jail as a **session**: a small server keeps the program in the jail alive when you detach, and a client attaches to it. This page is about that: detaching, the status bar and its colors, the window title, and what to do when a terminal misbehaves. How it is built is in [design.md](design.md#approval-prompts) and [milestones.md](../milestones.md).

## The status bar

`aicatraz up` keeps the bottom row of your terminal for a status bar: the project on the left, what runs and for how long in the middle (if there is room), and the detach key on the right. The program inside is told the terminal has one row less, and a scroll region keeps its output above the bar. It is a thin layer, not a terminal emulator; it is tested with vim and a shell. `--status-bar off` (or `AICATRAZ_STATUS_BAR=off`) turns it off, and is the first thing to try if something draws wrongly.

The bar is white on dark green (`forest`) by default. `aicatraz bar-colors` shows it in 18 ready-made themes (with their contrast, so you can see which are easy to read). `random` is not a theme but a draw: every time a session is attached it takes a different one of the others (so a re-attach may look different). Choose one, or set the colors yourself:

```bash
aicatraz up --bar-theme charcoal
aicatraz up --bar-theme amber --bar-fg '#102030'        # a theme, with another text color
aicatraz up --bar-fg bright-white --bar-bg 24           # your own: a name, 0-255, #rrggbb or default
```

Names and the numbers 0 to 15 follow your terminal's color theme; 16 to 255 and `#rrggbb` look the same everywhere. To keep a choice for yourself, put it in your shell config: `AICATRAZ_BAR_THEME`, `AICATRAZ_BAR_FG` and `AICATRAZ_BAR_BG`. To give one project its own look, so that its terminals are recognisable at a glance (a red bar for the project that deploys), put it in the project's config:

```yaml
bar:
  theme: brick          # a name from `aicatraz bar-colors`
  fg: white             # optional: text color, overrides the theme's
  bg: "#102030"         # optional: background, overrides the theme's
```

`Ctrl-Q` shows a popup with the session's settings: project, program, uptime, the theme the bar uses (for `random`, the one that was drawn, with a swatch), its colors and the config file. Any key closes it; the program inside does not see the keys and its output is held back meanwhile. Change the key with `--info-key ctrl-<key>` or `$AICATRAZ_INFO_KEY`, or turn the popup off with `--info-key off`. If it is the same key as the detach key, detaching wins and the popup is off.

**A prompt while you type.** An approval prompt (a network connection, a host command) that arrives while you are typing does not take over the screen, so a keystroke meant for the program can't answer it. The bar shows a blinking message instead, for example `⚠️ Network github.com:443  Ctrl-Q` or `⚠️ Command make deploy  Ctrl-Q` (it swaps between black on yellow and yellow on black). The prompt opens when you have not typed for `approval.idle` (5 seconds by default), or as soon as you press the info key. If you were idle when it came, it opens at once. Either way it ignores keys for the first `approval.guard` (500 ms), and its timeout starts when it opens. With `--info-key off` only the idle time opens it. `bar.symbol` and `bar.symbol_width` change the `⚠️` and the width your terminal draws it in. See [configuration.md](configuration.md#approval).

The theme, the text color and the background are each taken from the first place that sets them: a flag (`--bar-theme`, `--bar-fg`, `--bar-bg`), then the project's `bar` section, then your environment variables, then the default. So a project's look beats your personal default, and a flag beats both. The config is committed with the project, so a team shares it.

## Detaching, coming back, and a terminal that misbehaves

`Ctrl-Y` (or your `--detach-key`) leaves the session running. In the same project, `aicatraz up` attaches to it again, and so does `aicatraz claude` or `opencode` (their arguments are ignored while a session runs); `aicatraz down` stops it. There is one session per project. The session starts below the command that started it and leaves your screen as it was, so when it ends, or you detach, you are where you were. While you are attached, the window title is `AIcatraz <project>` (saved first, and given back when you leave); a title the program inside sets is ignored, or it would replace it. When you leave, the terminal is put back into plain modes: mouse reports, bracketed paste, keyboard protocols, cursor shape and so on are switched off, whatever the program turned on.

If a program still draws wrongly, or leaves the terminal in an odd state, `AICATRAZ_TERMLOG=/tmp/term.log aicatraz up ...` records everything sent to the terminal (`>`) and typed into it (`<`), with times, one quoted line each. It contains everything that was shown and typed, secrets included; share it with care. `reset` (the command) repairs a terminal by hand.
