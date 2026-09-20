#!/bin/bash

# Ensure the 'speculate' session exists
if ! tmux has-session -t speculate 2>/dev/null; then
  # Create a new session named 'speculate', detached
  cd /workspaces/seraphine
  tmux new-session -d -s speculate
  
  # Split the window horizontally (-h)
  # The left pane will remain a terminal
  # The right pane will run 'gh dash'
  tmux split-window -h -t speculate "gh dash"
fi
