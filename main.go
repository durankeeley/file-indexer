package main

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------
// MAIN ENTRY
// ---------------------------------------------

func main() {
	indexPath, err := getIndexFilePath()
	if err != nil {
		log.Fatalf("System error: %v", err)
	}

	// CLI: Force re-index
	if len(os.Args) > 1 && os.Args[1] == "index" {
		if err := buildIndex(indexPath); err != nil {
			log.Fatalf("Failed to build index: %v", err)
		}
		return
	}

	// Auto-setup: Build if missing
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Index not found in home folder. Running setup...")
		if err := buildIndex(indexPath); err != nil {
			log.Fatalf("Failed to build index: %v", err)
		}
	}

	files, err := loadIndex(indexPath)
	if err != nil {
		log.Fatalf("Failed to load index: %v", err)
	}

	if len(files) == 0 {
		fmt.Println("Index is empty. Try running `index` again.")
		return
	}

	p := tea.NewProgram(initialModel(files), tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		log.Fatalf("UI error: %v", err)
	}

	if m, ok := finalModel.(model); ok && m.selectedPath != "" {
		openFileLocation(m.selectedPath)
	}
}

// ---------------------------------------------
// UI MODEL
// ---------------------------------------------

type model struct {
	allFiles    []string
	matches     []string
	cursor      int
	windowStart int
	windowSize  int

	query        string
	selectedPath string
	width        int
	height       int
}

func initialModel(files []string) model {
	return model{
		allFiles:   files,
		matches:    nil,
		cursor:     0,
		windowSize: 15,
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msgTyped := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msgTyped.Width, msgTyped.Height
		if m.height > 5 {
			m.windowSize = m.height - 5
		}

	case tea.KeyMsg:
		switch msgTyped.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit

		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.windowStart {
					m.windowStart--
				}
			}

		case tea.KeyDown:
			if m.cursor < len(m.matches)-1 {
				m.cursor++
				if m.cursor >= m.windowStart+m.windowSize {
					m.windowStart++
				}
			}

		case tea.KeyEnter:
			if len(m.matches) > 0 {
				m.selectedPath = m.matches[m.cursor]
				return m, tea.Quit
			}

		case tea.KeyBackspace, tea.KeyDelete:
			if len(m.query) > 0 {
				m.query = m.query[:len(m.query)-1]
				m.performSearch()
			}

		case tea.KeyRunes:
			m.query += string(msgTyped.Runes)
			m.performSearch()

		case tea.KeySpace:
			m.query += " "
			m.performSearch()
		}
	}
	return m, nil
}

func (m *model) performSearch() {
	m.matches = m.matches[:0]
	m.cursor = 0
	m.windowStart = 0

	q := strings.ToLower(strings.TrimSpace(m.query))
	if q == "" {
		return
	}

	terms := strings.Fields(q)
	matchCount := 0

	for _, file := range m.allFiles {
		lower := strings.ToLower(file)
		matched := true
		for _, term := range terms {
			if !strings.Contains(lower, term) {
				matched = false
				break
			}
		}

		if matched {
			m.matches = append(m.matches, file)
			matchCount++
			if matchCount >= 1000 {
				break
			}
		}
	}
}

func (m model) View() string {
	var sb strings.Builder

	sb.WriteString("\n  Search (Esc to quit)\n")
	sb.WriteString(fmt.Sprintf("  > %s\u2588\n\n", m.query))

	if len(m.matches) == 0 && m.query != "" {
		sb.WriteString("  No matches found.\n")
		return sb.String()
	}

	end := m.windowStart + m.windowSize
	if end > len(m.matches) {
		end = len(m.matches)
	}

	for i := m.windowStart; i < end; i++ {
		cursor := " "
		line := m.matches[i]

		if i == m.cursor {
			cursor = ">"
			line = fmt.Sprintf("\033[1;36m%s\033[0m", line)
		}
		sb.WriteString(fmt.Sprintf("%s %s\n", cursor, line))
	}

	if len(m.matches) > 0 {
		sb.WriteString(fmt.Sprintf("\n  [Showing %d-%d of %d]\n",
			m.windowStart+1, end, len(m.matches)))
	}

	return sb.String()
}

// ---------------------------------------------
// INDEXING & FS LOGIC
// ---------------------------------------------

func getIndexFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find home directory: %w", err)
	}
	// Cross-platform path join (e.g. /home/user/.index or C:\Users\Name\.index)
	return filepath.Join(home, ".index"), nil
}

func buildIndex(savePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot get home directory: %w", err)
	}

	fmt.Println("Indexing home directory...")
	var files []string
	start := time.Now()

	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		// Security: Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		if len(files)%10000 == 0 {
			fmt.Printf("\rIndexed %d files...", len(files))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk error: %w", err)
	}

	fmt.Printf("\nFinished! Indexed %d files in %v\n", len(files), time.Since(start))
	return saveIndex(savePath, files)
}

func saveIndex(path string, files []string) error {
	// Security: 0600 = Read/Write by owner only
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create index file: %w", err)
	}
	defer f.Close()

	enc := gob.NewEncoder(f)
	return enc.Encode(files)
}

func loadIndex(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open index file: %w", err)
	}
	defer f.Close()

	var files []string
	dec := gob.NewDecoder(f)
	if err := dec.Decode(&files); err != nil {
		return nil, fmt.Errorf("invalid index: %w", err)
	}
	return files, nil
}

func openFileLocation(path string) {
	fmt.Printf("Revealing: %s\n", path)

	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("explorer", "/select,", path).Start()
	case "linux":
		switch {
		case isCmd("nautilus"):
			_ = exec.Command("nautilus", "--select", path).Start()
		case isCmd("dolphin"):
			_ = exec.Command("dolphin", "--select", path).Start()
		default:
			_ = exec.Command("xdg-open", filepath.Dir(path)).Start()
		}
	case "darwin":
		_ = exec.Command("open", "-R", path).Start()
	}
}

func isCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
