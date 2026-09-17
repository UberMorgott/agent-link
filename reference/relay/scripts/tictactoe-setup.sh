#!/bin/bash
#
# Tic-Tac-Toe Autonomous Agent Test
# Sets up two agents to play tic-tac-toe autonomously via file-based inbox
#

set -e

RELAY_DIR="${AGENT_RELAY_DIR:-/tmp/agent-relay-ttt}"
PLAYER_X="PlayerX"
PLAYER_O="PlayerO"

echo "=== Tic-Tac-Toe Agent Setup ==="
echo "Relay directory: $RELAY_DIR"
echo ""

# Create directories
mkdir -p "$RELAY_DIR/$PLAYER_X"
mkdir -p "$RELAY_DIR/$PLAYER_O"

# Clear any existing inbox
echo "" > "$RELAY_DIR/$PLAYER_X/inbox.md"
echo "" > "$RELAY_DIR/$PLAYER_O/inbox.md"

# Create game instructions for Player X (goes first)
cat > "$RELAY_DIR/$PLAYER_X/GAME_INSTRUCTIONS.md" << 'EOF'
# Tic-Tac-Toe Autonomous Game Protocol

You are **PlayerX** (X). You play FIRST. Your opponent is **PlayerO** (O).

## Board Positions
```
 1 | 2 | 3
-----------
 4 | 5 | 6
-----------
 7 | 8 | 9
```

## Commands Available
You have these `agent-relay` commands to communicate:

**Wait for opponent's message (blocking):**
```bash
agent-relay inbox-poll -n PlayerX -d /tmp/agent-relay-ttt --clear
```

**Send a move to opponent:**
```bash
agent-relay inbox-write -t PlayerO -f PlayerX -m "MOVE: X at position N" -d /tmp/agent-relay-ttt
```

## PROTOCOL (follow EXACTLY)

### Since you're X, you go FIRST:
1. Make your first move to position 5 (center)
2. Send it to opponent:
   ```bash
   agent-relay inbox-write -t PlayerO -f PlayerX -m "MOVE: X at position 5" -d /tmp/agent-relay-ttt
   ```

### Then enter the game loop:
1. **WAIT** for opponent's response (this will block until they respond):
   ```bash
   agent-relay inbox-poll -n PlayerX -d /tmp/agent-relay-ttt --clear
   ```

2. **UPDATE** your mental board state with opponent's move

3. **CHECK** for win/draw. If game over, send result and announce:
   ```bash
   agent-relay inbox-write -t PlayerO -f PlayerX -m "GAME OVER: X wins!" -d /tmp/agent-relay-ttt
   ```

4. **MAKE** your next move and send it:
   ```bash
   agent-relay inbox-write -t PlayerO -f PlayerX -m "MOVE: X at position N" -d /tmp/agent-relay-ttt
   ```

5. **REPEAT** from step 1 until game over

## Rules
- Valid moves: positions 1-9 that are empty
- Win: 3 in a row (horizontal, vertical, or diagonal)
- Draw: all 9 positions filled with no winner

## CRITICAL
- NEVER stop mid-game
- ALWAYS use the inbox-poll command to wait (it blocks until opponent responds)
- Keep track of the board state
- Announce result when game ends

## START NOW
Make your FIRST MOVE to position 5, then wait for opponent's response.
EOF

# Create game instructions for Player O (waits for X)
cat > "$RELAY_DIR/$PLAYER_O/GAME_INSTRUCTIONS.md" << 'EOF'
# Tic-Tac-Toe Autonomous Game Protocol

You are **PlayerO** (O). PlayerX plays first, so you WAIT for their move.

## Board Positions
```
 1 | 2 | 3
-----------
 4 | 5 | 6
-----------
 7 | 8 | 9
```

## Commands Available
You have these `agent-relay` commands to communicate:

**Wait for opponent's message (blocking):**
```bash
agent-relay inbox-poll -n PlayerO -d /tmp/agent-relay-ttt --clear
```

**Send a move to opponent:**
```bash
agent-relay inbox-write -t PlayerX -f PlayerO -m "MOVE: O at position N" -d /tmp/agent-relay-ttt
```

## PROTOCOL (follow EXACTLY)

### Since you're O, you go SECOND. Start by waiting:
1. **WAIT** for opponent's first move (this will block until they move):
   ```bash
   agent-relay inbox-poll -n PlayerO -d /tmp/agent-relay-ttt --clear
   ```

2. **UPDATE** your mental board state with opponent's move

3. **CHECK** for win/draw. If game over, announce result.

4. **MAKE** your response move and send it:
   ```bash
   agent-relay inbox-write -t PlayerX -f PlayerO -m "MOVE: O at position N" -d /tmp/agent-relay-ttt
   ```

5. **WAIT** for opponent's next move (back to step 1)

## Rules
- Valid moves: positions 1-9 that are empty
- Win: 3 in a row (horizontal, vertical, or diagonal)
- Draw: all 9 positions filled with no winner

## CRITICAL
- NEVER stop mid-game
- ALWAYS use the inbox-poll command to wait (it blocks until opponent responds)
- Keep track of the board state
- Announce result when game ends

## START NOW
Run the inbox-poll command to WAIT for PlayerX's first move.
EOF

echo "Created game instructions:"
echo "  - $RELAY_DIR/$PLAYER_X/GAME_INSTRUCTIONS.md"
echo "  - $RELAY_DIR/$PLAYER_O/GAME_INSTRUCTIONS.md"
echo ""
echo "=== TO START THE GAME ==="
echo ""
echo "Make sure agent-relay is built and in your PATH:"
echo "  cd $(dirname "$0")/.."
echo "  npm run build"
echo "  export PATH=\"\$PATH:\$(pwd)/dist/cli\""
echo ""
echo "Open TWO terminal windows and run Claude in each:"
echo ""
echo "Terminal 1 (PlayerX - goes first):"
echo "  claude"
echo "  # Then tell Claude: Read $RELAY_DIR/$PLAYER_X/GAME_INSTRUCTIONS.md and start the game"
echo ""
echo "Terminal 2 (PlayerO - waits):"
echo "  claude"
echo "  # Then tell Claude: Read $RELAY_DIR/$PLAYER_O/GAME_INSTRUCTIONS.md and start the game"
echo ""
echo "Both agents will autonomously play tic-tac-toe!"
echo ""
