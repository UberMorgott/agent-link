#!/bin/bash
#
# Hearts - 3 Player Autonomous Agent Game
# Sets up three agents to play Hearts autonomously via file-based inbox
#

set -e

GAME_DIR="${GAME_DIR:-/tmp/agent-relay-hearts}"
PLAYER_1="Alice"
PLAYER_2="Bob"
PLAYER_3="Carol"

echo "=== Hearts 3-Player Game Setup ==="
echo "Game directory: $GAME_DIR"
echo "Players: $PLAYER_1, $PLAYER_2, $PLAYER_3"
echo ""

# Create directories
mkdir -p "$GAME_DIR/$PLAYER_1"
mkdir -p "$GAME_DIR/$PLAYER_2"
mkdir -p "$GAME_DIR/$PLAYER_3"

# Clear any existing inbox
echo "" > "$GAME_DIR/$PLAYER_1/inbox.md"
echo "" > "$GAME_DIR/$PLAYER_2/inbox.md"
echo "" > "$GAME_DIR/$PLAYER_3/inbox.md"

# Generate and deal cards
# For 3 players: remove 2 of diamonds, deal 17 cards each
CARDS="2C 3C 4C 5C 6C 7C 8C 9C TC JC QC KC AC 2H 3H 4H 5H 6H 7H 8H 9H TH JH QH KH AH 2S 3S 4S 5S 6S 7S 8S 9S TS JS QS KS AS 3D 4D 5D 6D 7D 8D 9D TD JD QD KD AD"

# Shuffle cards using a simple method
SHUFFLED=$(echo $CARDS | tr ' ' '\n' | sort -R | tr '\n' ' ')
CARD_ARRAY=($SHUFFLED)

# Deal 17 cards to each player
HAND_1="${CARD_ARRAY[@]:0:17}"
HAND_2="${CARD_ARRAY[@]:17:17}"
HAND_3="${CARD_ARRAY[@]:34:17}"

# Create common game rules file
cat > "$GAME_DIR/GAME_RULES.md" << 'EOF'
# Hearts - 3 Player Rules

## Overview
Hearts is a trick-taking card game where you try to AVOID points.

## Card Values
- Suits: Clubs (C), Diamonds (D), Spades (S), Hearts (H)
- Ranks: 2-9, T(10), J, Q, K, A (low to high)
- Card notation: RankSuit (e.g., 2C = 2 of Clubs, QS = Queen of Spades)

## Scoring
- Each Heart: 1 point
- Queen of Spades (QS): 13 points
- **Goal: Get the LOWEST score**

## Special Rule: Shooting the Moon
If you take ALL hearts AND the Queen of Spades in one round:
- You score 0 points
- All opponents get 26 points each

## Gameplay

### 1. Passing Phase
- Pass 3 cards to the player on your left
- Round 1: Alice→Bob, Bob→Carol, Carol→Alice

### 2. Trick Phase
- Player with 2 of Clubs (2C) leads first trick
- Players must follow suit if able
- If can't follow suit, can play any card
- Highest card of the led suit wins the trick
- Winner leads next trick

### 3. Hearts Breaking
- Cannot lead hearts until hearts have been "broken"
- Hearts break when someone plays a heart (because they can't follow suit)

## Turn Order
Alice → Bob → Carol → Alice → ...

## End of Round
- Round ends when all 17 tricks are played
- Count points taken
- Game ends when someone reaches 100 points

## Commands

**Wait for your turn:**
```bash
agent-relay inbox-poll -n YOUR_NAME -d /tmp/agent-relay-hearts --clear
```

**Play a card (to next player or broadcast):**
```bash
agent-relay inbox-write -t "NEXT_PLAYER" -f YOUR_NAME -m "PLAY: CARD" -d /tmp/agent-relay-hearts
```

**Broadcast to all:**
```bash
agent-relay inbox-write -t "*" -f YOUR_NAME -m "MESSAGE" -d /tmp/agent-relay-hearts
```
EOF

# Create player-specific instruction files
create_player_instructions() {
  local PLAYER=$1
  local HAND=$2
  local PREV=$3
  local NEXT=$4
  local IS_FIRST=$5

  cat > "$GAME_DIR/$PLAYER/INSTRUCTIONS.md" << EOFPLAYER
# Hearts - You are $PLAYER

Read the game rules: \`cat /tmp/agent-relay-hearts/GAME_RULES.md\`

## Your Starting Hand (17 cards)
\`\`\`
$HAND
\`\`\`

## Players
- You: **$PLAYER**
- To your left (you pass TO): **$NEXT**
- To your right (you receive FROM): **$PREV**

## Commands

**Wait for messages:**
\`\`\`bash
agent-relay inbox-poll -n $PLAYER -d /tmp/agent-relay-hearts --clear
\`\`\`

**Send to next player:**
\`\`\`bash
agent-relay inbox-write -t "$NEXT" -f $PLAYER -m "YOUR_MESSAGE" -d /tmp/agent-relay-hearts
\`\`\`

**Broadcast to all:**
\`\`\`bash
agent-relay inbox-write -t "*" -f $PLAYER -m "YOUR_MESSAGE" -d /tmp/agent-relay-hearts
\`\`\`

## Game Protocol

### Phase 1: Card Passing
1. Choose 3 cards to pass to $NEXT
2. Send: \`PASS: card1 card2 card3\`
3. Wait to receive 3 cards from $PREV
4. Update your hand (remove passed, add received)

### Phase 2: Playing Tricks
The player with 2C leads the first trick.

**When it's your turn to play:**
1. Look at what's been played in this trick
2. If you have the led suit, you MUST play it
3. If you don't have the led suit, play any card
4. Send: \`PLAY: CARD\` (e.g., \`PLAY: 7H\`)
5. If you're last in trick, also announce winner and next lead

**Message format for playing:**
- \`PLAY: CARD\` - Your card play
- \`TRICK_WINNER: PLAYER\` - Who won the trick
- \`POINTS: {Alice: X, Bob: Y, Carol: Z}\` - Point totals

### Tracking State
Keep track of:
- Your current hand
- Cards played this trick
- Who has played
- Hearts broken? (true/false)
- Points taken this round

EOFPLAYER

  if [ "$IS_FIRST" = "true" ]; then
    cat >> "$GAME_DIR/$PLAYER/INSTRUCTIONS.md" << EOFSTART

## You Start!
Since you are $PLAYER, you coordinate the game start:

1. **First, wait for all pass cards:**
   \`\`\`bash
   agent-relay inbox-poll -n $PLAYER -d /tmp/agent-relay-hearts --clear
   \`\`\`

2. **Send your 3 pass cards to $NEXT:**
   \`\`\`bash
   agent-relay inbox-write -t "$NEXT" -f $PLAYER -m "PASS: [card1] [card2] [card3]" -d /tmp/agent-relay-hearts
   \`\`\`

3. **Wait for pass cards from $PREV**

4. **After everyone has passed, if you have 2C, lead first trick:**
   \`\`\`bash
   agent-relay inbox-write -t "*" -f $PLAYER -m "TRICK 1 - $PLAYER leads: PLAY: 2C" -d /tmp/agent-relay-hearts
   \`\`\`

5. **Wait for responses and continue the game loop**

## START NOW
Begin by choosing 3 cards to pass and sending them to $NEXT!
EOFSTART
  else
    cat >> "$GAME_DIR/$PLAYER/INSTRUCTIONS.md" << EOFWAIT

## Waiting for Game Start
You will receive the first message when it's time to pass cards.

1. **Wait for instruction to pass:**
   \`\`\`bash
   agent-relay inbox-poll -n $PLAYER -d /tmp/agent-relay-hearts --clear
   \`\`\`

2. **When prompted, send your 3 pass cards to $NEXT:**
   \`\`\`bash
   agent-relay inbox-write -t "$NEXT" -f $PLAYER -m "PASS: [card1] [card2] [card3]" -d /tmp/agent-relay-hearts
   \`\`\`

3. **Continue following the game protocol**

## START NOW
Run the inbox-poll command to wait for the game to begin!
EOFWAIT
  fi
}

# Create instructions for each player
create_player_instructions "$PLAYER_1" "$HAND_1" "$PLAYER_3" "$PLAYER_2" "true"
create_player_instructions "$PLAYER_2" "$HAND_2" "$PLAYER_1" "$PLAYER_3" "false"
create_player_instructions "$PLAYER_3" "$HAND_3" "$PLAYER_2" "$PLAYER_1" "false"

echo "Created game files:"
echo "  - $GAME_DIR/GAME_RULES.md"
echo "  - $GAME_DIR/$PLAYER_1/INSTRUCTIONS.md (Hand: $HAND_1)"
echo "  - $GAME_DIR/$PLAYER_2/INSTRUCTIONS.md (Hand: $HAND_2)"
echo "  - $GAME_DIR/$PLAYER_3/INSTRUCTIONS.md (Hand: $HAND_3)"
echo ""
echo "=== TO START THE GAME ==="
echo ""
echo "Make sure agent-relay is built:"
echo "  cd $(dirname "$0")/../.."
echo "  npm run build"
echo ""
echo "Open THREE terminal windows and run Claude/Codex in each:"
echo ""
echo "Terminal 1 ($PLAYER_1 - coordinates start):"
echo "  claude"
echo "  # Say: Read $GAME_DIR/$PLAYER_1/INSTRUCTIONS.md and start the Hearts game"
echo ""
echo "Terminal 2 ($PLAYER_2):"
echo "  claude"
echo "  # Say: Read $GAME_DIR/$PLAYER_2/INSTRUCTIONS.md and play Hearts"
echo ""
echo "Terminal 3 ($PLAYER_3):"
echo "  claude"
echo "  # Say: Read $GAME_DIR/$PLAYER_3/INSTRUCTIONS.md and play Hearts"
echo ""
echo "The three agents will autonomously play Hearts!"
echo ""
