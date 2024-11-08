package main

import (
    "bufio"
    "crypto/tls"
    "database/sql"
    "fmt"
    "log"
    "os"
    "os/user"
    "path/filepath"
    "regexp"
    "strings"

    _ "github.com/mattn/go-sqlite3"
    irc "github.com/thoj/go-ircevent"
)

// Config struct holds configuration settings from .tally.conf
type Config struct {
    Nickname string
    Server   string
    Channel  string
    UseTLS   bool
}

type TallyBot struct {
    conn    *irc.Connection
    db      *sql.DB
    channel string
}

func NewTallyBot(nick, server, channel string, useTLS bool) *TallyBot {
    conn := irc.IRC(nick, nick)
    conn.UseTLS = useTLS
    if useTLS {
        conn.TLSConfig = &tls.Config{
            InsecureSkipVerify: true,
        }
    }
    bot := &TallyBot{
        conn:    conn,
        channel: channel,
    }
    return bot
}

func (bot *TallyBot) initializeDatabase() {
    var err error
    bot.db, err = sql.Open("sqlite3", "./tallies.db")
    if err != nil {
        log.Fatal(err)
    }

    // Create tables if they don't exist
    statements := []string{
        `CREATE TABLE IF NOT EXISTS tallies (
            item TEXT PRIMARY KEY,
            score INTEGER
        );`,
        `CREATE TABLE IF NOT EXISTS aliases (
            item TEXT PRIMARY KEY,
            group_id INTEGER
        );`,
        `CREATE TABLE IF NOT EXISTS groups (
            group_id INTEGER PRIMARY KEY AUTOINCREMENT
        );`,
    }

    for _, stmt := range statements {
        _, err := bot.db.Exec(stmt)
        if err != nil {
            log.Fatal(err)
        }
    }
}

func (bot *TallyBot) ensureItemExists(item string) {
    // Check if item exists in tallies
    var count int
    err := bot.db.QueryRow("SELECT COUNT(*) FROM tallies WHERE item = ?", item).Scan(&count)
    if err != nil {
        log.Println(err)
        return
    }

    if count == 0 {
        // Insert item into tallies
        _, err := bot.db.Exec("INSERT INTO tallies (item, score) VALUES (?, 0)", item)
        if err != nil {
            log.Println(err)
            return
        }
        // Insert new group
        res, err := bot.db.Exec("INSERT INTO groups DEFAULT VALUES")
        if err != nil {
            log.Println(err)
            return
        }
        groupID, err := res.LastInsertId()
        if err != nil {
            log.Println(err)
            return
        }
        // Insert into aliases
        _, err = bot.db.Exec("INSERT INTO aliases (item, group_id) VALUES (?, ?)", item, groupID)
        if err != nil {
            log.Println(err)
            return
        }
    }
}

func (bot *TallyBot) getScore(item string) int {
    var score int
    err := bot.db.QueryRow("SELECT score FROM tallies WHERE item = ?", item).Scan(&score)
    if err != nil {
        log.Println(err)
        return 0
    }
    return score
}

func (bot *TallyBot) updateScore(item string, score int) {
    _, err := bot.db.Exec("UPDATE tallies SET score = ? WHERE item = ?", score, item)
    if err != nil {
        log.Println(err)
    }
}

func (bot *TallyBot) getGroupID(item string) int {
    var groupID int
    err := bot.db.QueryRow("SELECT group_id FROM aliases WHERE item = ?", item).Scan(&groupID)
    if err != nil {
        // Item not found
        return 0
    }
    return groupID
}

func (bot *TallyBot) linkItems(item1, item2 string) {
    bot.ensureItemExists(item1)
    bot.ensureItemExists(item2)
    group1 := bot.getGroupID(item1)
    group2 := bot.getGroupID(item2)
    if group1 != group2 {
        // Merge groups
        _, err := bot.db.Exec("UPDATE aliases SET group_id = ? WHERE group_id = ?", group1, group2)
        if err != nil {
            log.Println(err)
            return
        }
        // Remove old group
        _, err = bot.db.Exec("DELETE FROM groups WHERE group_id = ?", group2)
        if err != nil {
            log.Println(err)
            return
        }
    }
}

func (bot *TallyBot) unlinkItems(item1, item2 string) {
    bot.ensureItemExists(item1)
    bot.ensureItemExists(item2)
    group1 := bot.getGroupID(item1)
    group2 := bot.getGroupID(item2)
    if group1 == group2 {
        // Create new group for item2
        res, err := bot.db.Exec("INSERT INTO groups DEFAULT VALUES")
        if err != nil {
            log.Println(err)
            return
        }
        newGroupID, err := res.LastInsertId()
        if err != nil {
            log.Println(err)
            return
        }
        _, err = bot.db.Exec("UPDATE aliases SET group_id = ? WHERE item = ?", newGroupID, item2)
        if err != nil {
            log.Println(err)
            return
        }
    }
}

func (bot *TallyBot) getTotalScore(item string) int {
    groupID := bot.getGroupID(item)
    if groupID == 0 {
        return bot.getScore(item)
    }
    var total int
    err := bot.db.QueryRow(`
        SELECT SUM(score) FROM tallies
        JOIN aliases USING(item)
        WHERE group_id = ?`, groupID).Scan(&total)
    if err != nil {
        log.Println(err)
        return 0
    }
    return total
}

func (bot *TallyBot) getLinkedItems(item string) []string {
    groupID := bot.getGroupID(item)
    if groupID == 0 {
        return []string{}
    }
    rows, err := bot.db.Query("SELECT item FROM aliases WHERE group_id = ? AND item != ?", groupID, item)
    if err != nil {
        log.Println(err)
        return []string{}
    }
    defer rows.Close()
    var items []string
    for rows.Next() {
        var linkedItem string
        err := rows.Scan(&linkedItem)
        if err != nil {
            log.Println(err)
            continue
        }
        items = append(items, linkedItem)
    }
    return items
}

func (bot *TallyBot) handleMessage(nick, message string) {
    message = strings.TrimSpace(message)
    messageLower := strings.ToLower(message)

    // Regex patterns
    linkRegex := regexp.MustCompile(`^!link ([\w\.]+) ([\w\.]+)$`)
    unlinkRegex := regexp.MustCompile(`^!unlink ([\w\.]+) ([\w\.]+)$`)
    totalRegex := regexp.MustCompile(`^!total ([\w\.]+)$`)
    upvoteRegex := regexp.MustCompile(`([\w\.]+)(\+\+|--)`)

    // Handle unlinking
    if unlinkMatch := unlinkRegex.FindStringSubmatch(messageLower); unlinkMatch != nil {
        item1 := unlinkMatch[1]
        item2 := unlinkMatch[2]
        bot.unlinkItems(item1, item2)
        response := fmt.Sprintf("Unlinked %s and %s.", item1, item2)
        bot.conn.Privmsg(bot.channel, response)
        return
    }

    // Handle linking
    if linkMatch := linkRegex.FindStringSubmatch(messageLower); linkMatch != nil {
        item1 := linkMatch[1]
        item2 := linkMatch[2]
        bot.linkItems(item1, item2)
        response := fmt.Sprintf("Linked %s and %s.", item1, item2)
        bot.conn.Privmsg(bot.channel, response)
        return
    }

    // Handle total score query
    if totalMatch := totalRegex.FindStringSubmatch(messageLower); totalMatch != nil {
        item := totalMatch[1]
        totalScore := bot.getTotalScore(item)
        response := fmt.Sprintf("Total score for group including %s: [%d]", item, totalScore)
        bot.conn.Privmsg(bot.channel, response)
        return
    }

    // Handle upvote/downvote within the message
    if upvoteMatches := upvoteRegex.FindAllStringSubmatch(message, -1); upvoteMatches != nil {
        for _, match := range upvoteMatches {
            item := strings.ToLower(match[1])
            operation := match[2]
            bot.ensureItemExists(item)
            currentScore := bot.getScore(item)
            var newScore int
            if operation == "++" {
                newScore = currentScore + 1
            } else {
                newScore = currentScore - 1
            }
            bot.updateScore(item, newScore)
            linkedItems := bot.getLinkedItems(item)
            // Prepare linked items string
            var linkedStr string
            if len(linkedItems) > 0 {
                linkedStr = fmt.Sprintf(" (linked with: %s)", strings.Join(linkedItems, ", "))
            }
            // Respond with item's score and linked items
            response := fmt.Sprintf("%s: [%d]%s", item, newScore, linkedStr)
            bot.conn.Privmsg(bot.channel, response)
        }
        return
    }
}

// readConfig reads configuration from .tally.conf
func readConfig() (Config, error) {
    var config Config
    var configPaths []string

    // First, look for .tally.conf in the current directory
    configPaths = append(configPaths, ".tally.conf")

    // Then, look for .tally.conf in the user's home directory
    usr, err := user.Current()
    if err == nil {
        homeConfigPath := filepath.Join(usr.HomeDir, ".tally.conf")
        configPaths = append(configPaths, homeConfigPath)
    }

    var file *os.File
    for _, path := range configPaths {
        f, err := os.Open(path)
        if err == nil {
            file = f
            defer file.Close()
            break
        }
    }

    if file == nil {
        return config, fmt.Errorf("configuration file .tally.conf not found")
    }

    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()
        // Remove comments and whitespace
        line = strings.TrimSpace(line)
        if len(line) == 0 || strings.HasPrefix(line, "#") {
            continue
        }
        // Split key and value
        parts := strings.SplitN(line, "=", 2)
        if len(parts) != 2 {
            continue
        }
        key := strings.TrimSpace(parts[0])
        value := strings.TrimSpace(parts[1])
        switch key {
        case "nickname":
            config.Nickname = value
        case "server":
            config.Server = value
        case "channel":
            config.Channel = value
        case "use_tls":
            valueLower := strings.ToLower(value)
            config.UseTLS = valueLower == "true" || valueLower == "yes" || valueLower == "1"
        default:
            // Unknown key, ignore or log
            fmt.Printf("Unknown configuration key: %s\n", key)
        }
    }
    if err := scanner.Err(); err != nil {
        return config, err
    }

    // Set default values if not set
    if config.Nickname == "" {
        config.Nickname = "TallyBot"
    }
    if config.Server == "" {
        config.Server = "irc.libera.chat:6667"
    }
    if config.Channel == "" {
        config.Channel = "#tallybot"
    }

    return config, nil
}

func main() {
    config, err := readConfig()
    if err != nil {
        fmt.Printf("Error reading configuration: %v\n", err)
        return
    }

    fmt.Printf("Starting bot with nickname '%s' on server '%s', joining channel '%s'\n",
        config.Nickname, config.Server, config.Channel)

    bot := NewTallyBot(config.Nickname, config.Server, config.Channel, config.UseTLS)
    bot.initializeDatabase()

    // Add handlers
    bot.conn.AddCallback("001", func(e *irc.Event) {
        bot.conn.Join(bot.channel)
    })

    bot.conn.AddCallback("PRIVMSG", func(e *irc.Event) {
        nick := e.Nick
        message := e.Message()
        bot.handleMessage(nick, message)
    })

    // Connect to server
    err = bot.conn.Connect(config.Server)
    if err != nil {
        fmt.Printf("Failed to connect to IRC server: %s\n", err)
        return
    }

    // Start the bot
    bot.conn.Loop()
}
