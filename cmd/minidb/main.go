// minidb is an interactive shell for the minidb database.
//
//	minidb -db my.db              interactive shell
//	minidb -db my.db -e "..."     run statements and exit
//	minidb -db my.db -f file.sql  run a script and exit
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/subhaushsingh/goDB"
)

func main() {
	path := flag.String("db", "minidb.data", "database file")
	expr := flag.String("e", "", "execute statements and exit")
	file := flag.String("f", "", "execute a SQL file and exit")
	flag.Parse()

	db := &minidb.DB{Path: *path}
	if err := db.Open(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer db.Close()
	s := db.NewSession()
	defer s.Close()

	switch {
	case *expr != "":
		os.Exit(execute(s, *expr))
	case *file != "":
		src, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(execute(s, string(src)))
	}
	repl(s, *path)
}

func execute(s *minidb.Session, src string) int {
	results, err := s.Exec(src)
	for _, r := range results {
		printResult(r)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

const help = `Statements end with ';'.

  create table t (id int64, name string, index (name), primary key (id));
  insert into t (id, name) values (1, 'a'), (2, 'b');
  select id, name from t index by id > 0 filter name <> 'b' limit 10;
  select * from t index by id < 5 and id > 1;      -- descending range
  update t set name = name + '!' filter id = 1;
  delete from t index by id = 2;
  show tables;   describe t;
  begin;  commit;  rollback;

Dot commands: .help  .exit`

func repl(s *minidb.Session, path string) {
	fmt.Printf("minidb — %s\nType .help for help, .exit to quit.\n", path)
	in := bufio.NewReader(os.Stdin)
	var buf strings.Builder
	for {
		switch {
		case buf.Len() > 0:
			fmt.Print("   ...> ")
		case s.InTx():
			fmt.Print("minidb*> ")
		default:
			fmt.Print("minidb> ")
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Println()
			return
		}
		trimmed := strings.TrimSpace(line)
		if buf.Len() == 0 && strings.HasPrefix(trimmed, ".") {
			switch trimmed {
			case ".exit", ".quit":
				return
			case ".help":
				fmt.Println(help)
			default:
				fmt.Println("unknown command:", trimmed)
			}
			continue
		}
		buf.WriteString(line)
		if complete(buf.String()) {
			execute(s, buf.String())
			buf.Reset()
		}
	}
}

// does the input end with a `;` outside of a string / comment?
func complete(src string) bool {
	var quote byte
	last := byte(0)
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		default:
			last = c
		}
	}
	return quote == 0 && last == ';'
}

func printResult(r *minidb.QLResult) {
	switch {
	case r.Cols != nil:
		printTable(r)
		fmt.Printf("(%d rows)\n", len(r.Rows))
	case r.Message != "":
		fmt.Println(r.Message)
	default:
		fmt.Printf("OK, %d rows affected\n", r.Affected)
	}
}

func printTable(r *minidb.QLResult) {
	cells := make([][]string, len(r.Rows))
	widths := make([]int, len(r.Cols))
	for i, c := range r.Cols {
		widths[i] = utf8.RuneCountInString(c)
	}
	for i, row := range r.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			s := strings.NewReplacer("\n", "\\n", "\t", "\\t").Replace(v.String())
			cells[i][j] = s
			widths[j] = max(widths[j], utf8.RuneCountInString(s))
		}
	}
	line := func(vals []string) {
		for j, v := range vals {
			if j > 0 {
				fmt.Print(" | ")
			}
			fmt.Print(v, strings.Repeat(" ", widths[j]-utf8.RuneCountInString(v)))
		}
		fmt.Println()
	}
	line(r.Cols)
	seps := make([]string, len(widths))
	for j, w := range widths {
		seps[j] = strings.Repeat("-", w)
	}
	fmt.Println(strings.Join(seps, "-+-"))
	for _, row := range cells {
		line(row)
	}
}
