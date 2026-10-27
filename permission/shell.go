package permission

import (
	"path/filepath"
	"strings"
)

// 本文件实现一个"够用且保守"的 shell 命令分析器，用于 bash 权限规则匹配。
//
// v0.1.0 直接对整条命令做前缀匹配：allow 规则 bash(go test:*) 会放行
// `go test ./... && rm -rf ~`，deny 规则 bash(rm -rf:*) 挡不住 `cd x && rm -rf y`。
// 现在先把命令拆成子命令（按 && || ; | & 换行、子 shell 括号拆分，尊重引号与转义），再：
//
//   - allow：必须**每一个**子命令都命中规则；
//   - deny / ask：**任意一个**子命令（包括 $(...)、eval、sh -c 等内部命令）命中即生效；
//   - 无法安全分析的结构（命令替换、eval、heredoc、变量做命令名、循环……）不允许被 allow 规则放行，
//     最终落到"询问用户"。

// Redirect 是一个重定向。
type Redirect struct {
	Op     string // > >> < << <<< >| &> &>> >& <&
	Target string
}

// SubCommand 是拆分出的一个简单命令。
type SubCommand struct {
	// Assigns 是命令前的变量赋值（如 FOO=1 go test 中的 FOO=1）。
	Assigns []string
	// Words 是去掉引号后的单词，Words[0] 是命令名。
	Words     []string
	Redirects []Redirect
}

// String 返回规范化后的命令文本（单词以单个空格连接，不含赋值与重定向）。
func (c SubCommand) String() string { return strings.Join(c.Words, " ") }

// Analysis 是一条 shell 命令的分析结果。
type Analysis struct {
	// Commands 是顶层可直接看到的子命令（allow 规则需要全部命中）。
	Commands []SubCommand
	// Nested 是藏在 $(...)、反引号、eval、sh -c、xargs/sudo 等包装命令中的命令，只用于 deny/ask 检查。
	Nested []SubCommand
	// Unsafe 非空表示命令包含无法安全分析的结构，allow 规则不能放行它。
	Unsafe []string
}

// Safe 报告命令是否可以被完整分析。
func (a *Analysis) Safe() bool { return len(a.Unsafe) == 0 }

func (a *Analysis) unsafe(reason string) {
	for _, r := range a.Unsafe {
		if r == reason {
			return
		}
	}
	a.Unsafe = append(a.Unsafe, reason)
}

// All 返回顶层与嵌套的全部子命令。
func (a *Analysis) All() []SubCommand {
	return append(append([]SubCommand(nil), a.Commands...), a.Nested...)
}

// AnalyzeCommand 分析一条 shell 命令。
func AnalyzeCommand(cmd string) *Analysis {
	a := &Analysis{}
	analyze(cmd, a, 0, false)
	return a
}

const maxDepth = 4

// analyze 解析 cmd，nested 为 true 时子命令进入 a.Nested。
func analyze(cmd string, a *Analysis, depth int, nested bool) {
	if depth > maxDepth {
		a.unsafe("嵌套层级过深")
		return
	}
	p := &lexer{src: cmd, a: a, depth: depth}
	p.run()
	for _, c := range p.cmds {
		c = expandWrappers(c, a, depth)
		if len(c.Words) == 0 {
			continue
		}
		if nested {
			a.Nested = append(a.Nested, c)
		} else {
			a.Commands = append(a.Commands, c)
		}
	}
}

// 会把参数当作命令执行的"包装"命令：sudo rm -rf x 实际执行的是 rm -rf x。
var wrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "nohup": true, "nice": true, "time": true, "timeout": true,
	"command": true, "builtin": true, "exec": true, "xargs": true, "stdbuf": true, "ionice": true,
	"chroot": true, "unbuffer": true, "caffeinate": true, "watch": true, "strace": true, "flock": true,
}

// 把字符串参数当作 shell 代码执行的命令。
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// expandWrappers 处理 eval / sh -c / sudo 等：内部命令进入 Nested，并把外层标记为不可安全分析。
func expandWrappers(c SubCommand, a *Analysis, depth int) SubCommand {
	if len(c.Words) == 0 {
		return c
	}
	name := filepath.Base(c.Words[0])
	switch {
	case name == "eval" || name == "source" || name == ".":
		a.unsafe("使用了 " + name)
		analyze(strings.Join(c.Words[1:], " "), a, depth+1, true)
	case shells[name]:
		for i := 1; i < len(c.Words); i++ {
			w := c.Words[i]
			if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "c") && i+1 < len(c.Words) {
				a.unsafe("通过 " + name + " -c 执行字符串")
				analyze(c.Words[i+1], a, depth+1, true)
				break
			}
		}
	case wrappers[name]:
		// 包装命令的选项可能带参数（sudo -u root rm ...），无法可靠地知道被执行的命令从哪个单词开始，
		// 于是把每一个"非选项单词开头的后缀"都当作候选，交给 deny / ask 规则检查（宁可多拦）。
		for i := 1; i < len(c.Words); i++ {
			w := c.Words[i]
			if strings.HasPrefix(w, "-") || strings.Contains(w, "=") || isDurationLike(w) {
				continue
			}
			inner := expandWrappers(SubCommand{Words: c.Words[i:]}, a, depth+1)
			a.Nested = append(a.Nested, inner)
		}
	case name == "find":
		for i, w := range c.Words {
			if (w == "-exec" || w == "-execdir" || w == "-ok" || w == "-okdir") && i+1 < len(c.Words) {
				end := len(c.Words)
				for j := i + 1; j < len(c.Words); j++ {
					if c.Words[j] == ";" || c.Words[j] == "+" || c.Words[j] == `\;` {
						end = j
						break
					}
				}
				a.Nested = append(a.Nested, SubCommand{Words: c.Words[i+1 : end]})
			}
		}
	}
	return c
}

func isDurationLike(w string) bool {
	if w == "" {
		return false
	}
	w = strings.TrimRight(w, "smhd")
	for _, r := range w {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return w != ""
}

// 在命令位置出现时需要特殊处理的保留字。
var (
	skipWords   = map[string]bool{"then": true, "else": true, "elif": true, "do": true, "!": true, "{": true, "if": true, "while": true, "until": true}
	endWords    = map[string]bool{"fi": true, "done": true, "}": true, "esac": true}
	unsafeWords = map[string]bool{"for": true, "case": true, "select": true, "function": true, "coproc": true}
)

// lexer 是一个最小的 shell 词法/语法分析器。
type lexer struct {
	src   string
	pos   int
	a     *Analysis
	depth int

	cmds []SubCommand
	cur  SubCommand
	word strings.Builder
	// inWord 表示当前单词已经开始（即使内容为空，例如 ""）。
	inWord bool
	// wordHasExpansion 表示当前单词含有 $ 展开或通配符。
	wordHasExpansion bool
	// pendingRedirect 非空表示下一个单词是重定向目标。
	pendingRedirect string
}

func (l *lexer) peek(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

// endWord 结束当前单词。
func (l *lexer) endWord() {
	if !l.inWord {
		return
	}
	w := l.word.String()
	l.word.Reset()
	l.inWord = false
	exp := l.wordHasExpansion
	l.wordHasExpansion = false
	if l.pendingRedirect != "" {
		l.cur.Redirects = append(l.cur.Redirects, Redirect{Op: l.pendingRedirect, Target: w})
		l.pendingRedirect = ""
		return
	}
	if len(l.cur.Words) == 0 {
		// 命令位置：变量赋值、保留字
		if i := strings.IndexByte(w, '='); i > 0 && isName(w[:i]) {
			l.cur.Assigns = append(l.cur.Assigns, w)
			return
		}
		switch {
		case skipWords[w]:
			return
		case endWords[w]:
			return
		case unsafeWords[w]:
			l.a.unsafe("使用了 " + w + " 语句")
			return
		}
		if exp {
			l.a.unsafe("命令名包含变量或通配符")
		}
	}
	l.cur.Words = append(l.cur.Words, w)
}

func isName(s string) bool {
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return s != ""
}

// endCommand 结束当前子命令。
func (l *lexer) endCommand() {
	l.endWord()
	if l.pendingRedirect != "" {
		l.a.unsafe("重定向缺少目标")
		l.pendingRedirect = ""
	}
	if len(l.cur.Words) > 0 || len(l.cur.Redirects) > 0 || len(l.cur.Assigns) > 0 {
		if len(l.cur.Words) == 0 && len(l.cur.Redirects) > 0 {
			// 形如 `> file` 的纯重定向也会写文件
			l.cur.Words = []string{":"}
		}
		if len(l.cur.Words) > 0 {
			l.cmds = append(l.cmds, l.cur)
		}
	}
	l.cur = SubCommand{}
}

// readUntilClose 读取与 open 配对的 close 之间的内容（处理嵌套与引号），返回内容。
func (l *lexer) readBalanced(open, close byte) (string, bool) {
	depth := 1
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '\\':
			l.pos += 2
			continue
		case '\'':
			if j := strings.IndexByte(l.src[l.pos+1:], '\''); j >= 0 {
				l.pos += j + 2
				continue
			}
			l.pos = len(l.src)
			return l.src[start:], false
		case '"':
			l.pos++
			for l.pos < len(l.src) && l.src[l.pos] != '"' {
				if l.src[l.pos] == '\\' {
					l.pos++
				}
				l.pos++
			}
			l.pos++
			continue
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				s := l.src[start:l.pos]
				l.pos++
				return s, true
			}
		}
		l.pos++
	}
	return l.src[start:], false
}

// substitution 处理 $( ... ) 与反引号：内部命令进入 Nested，并标记不可安全分析。
func (l *lexer) substitution(inner string, kind string) {
	l.a.unsafe("包含" + kind)
	analyze(inner, l.a, l.depth+1, true)
	l.inWord = true
	l.wordHasExpansion = true
	l.word.WriteString("$(...)")
}

func (l *lexer) run() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\\':
			if l.peek(1) == '\n' { // 续行
				l.pos += 2
				continue
			}
			if l.pos+1 < len(l.src) {
				l.word.WriteByte(l.src[l.pos+1])
			}
			l.inWord = true
			l.pos += 2
		case c == '\'':
			j := strings.IndexByte(l.src[l.pos+1:], '\'')
			if j < 0 {
				l.a.unsafe("引号不配对")
				l.word.WriteString(l.src[l.pos+1:])
				l.inWord = true
				l.pos = len(l.src)
				continue
			}
			l.word.WriteString(l.src[l.pos+1 : l.pos+1+j])
			l.inWord = true
			l.pos += j + 2
		case c == '"':
			l.doubleQuoted()
		case c == '`':
			l.pos++
			j := strings.IndexByte(l.src[l.pos:], '`')
			if j < 0 {
				l.a.unsafe("反引号不配对")
				j = len(l.src) - l.pos
			}
			inner := l.src[l.pos : l.pos+j]
			l.pos += j + 1
			l.substitution(inner, "反引号命令替换")
		case c == '$':
			l.dollar()
		case c == '#' && !l.inWord:
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == ' ' || c == '\t' || c == '\r':
			l.endWord()
			l.pos++
		case c == '\n' || c == ';':
			l.endCommand()
			l.pos++
		case c == '&':
			switch {
			case l.peek(1) == '&':
				l.endCommand()
				l.pos += 2
			case l.peek(1) == '>':
				l.endWord()
				op := "&>"
				l.pos += 2
				if l.peek(0) == '>' {
					op = "&>>"
					l.pos++
				}
				l.pendingRedirect = op
			default:
				l.endCommand()
				l.pos++
			}
		case c == '|':
			l.endCommand()
			l.pos++
			if l.peek(0) == '|' || l.peek(0) == '&' {
				l.pos++
			}
		case c == '(' || c == ')':
			if c == '(' && l.inWord {
				l.a.unsafe("包含函数定义或数组")
			}
			if c == '(' && l.peek(1) == '(' {
				l.a.unsafe("包含算术表达式")
			}
			l.endCommand()
			l.pos++
		case c == '>' || c == '<':
			l.redirect()
		case (c == '*' || c == '?' || c == '[') && len(l.cur.Words) == 0 && l.pendingRedirect == "":
			l.wordHasExpansion = true
			l.word.WriteByte(c)
			l.inWord = true
			l.pos++
		default:
			l.word.WriteByte(c)
			l.inWord = true
			l.pos++
		}
	}
	l.endCommand()
}

func (l *lexer) doubleQuoted() {
	l.pos++
	l.inWord = true
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.pos++
			return
		case '\\':
			if n := l.peek(1); n == '"' || n == '\\' || n == '$' || n == '`' {
				l.word.WriteByte(n)
				l.pos += 2
				continue
			}
			l.word.WriteByte(c)
			l.pos++
		case '`':
			l.pos++
			j := strings.IndexByte(l.src[l.pos:], '`')
			if j < 0 {
				j = len(l.src) - l.pos
			}
			inner := l.src[l.pos : l.pos+j]
			l.pos += j + 1
			l.substitution(inner, "反引号命令替换")
		case '$':
			l.dollar()
		default:
			l.word.WriteByte(c)
			l.pos++
		}
	}
	l.a.unsafe("引号不配对")
}

// dollar 处理 $ 开头的展开。
func (l *lexer) dollar() {
	switch n := l.peek(1); {
	case n == '(':
		l.pos += 2
		if l.peek(0) == '(' { // $(( 算术 ))
			l.pos++
			inner, _ := l.readBalanced('(', ')')
			l.pos++ // 第二个 )
			if strings.ContainsAny(inner, "$`") {
				l.a.unsafe("算术表达式中包含展开")
			}
			l.inWord, l.wordHasExpansion = true, true
			l.word.WriteString("$((...))")
			return
		}
		inner, ok := l.readBalanced('(', ')')
		if !ok {
			l.a.unsafe("括号不配对")
		}
		l.substitution(inner, "命令替换 $(...)")
	case n == '{':
		l.pos += 2
		inner, _ := l.readBalanced('{', '}')
		if strings.ContainsAny(inner, "$`(") {
			l.a.unsafe("参数展开中包含命令")
			analyze(inner, l.a, l.depth+1, true)
		}
		l.inWord, l.wordHasExpansion = true, true
		l.word.WriteString("${" + inner + "}")
	case n == '\'': // $'...' ANSI-C 引号
		l.pos += 2
		j := strings.IndexByte(l.src[l.pos:], '\'')
		if j < 0 {
			j = len(l.src) - l.pos
		}
		l.word.WriteString(l.src[l.pos : l.pos+j])
		l.inWord = true
		l.pos += j + 1
		l.a.unsafe("包含 $'...' 转义字符串")
	default:
		l.word.WriteByte('$')
		l.inWord, l.wordHasExpansion = true, true
		l.pos++
	}
}

// redirect 处理 > >> < << <<< >| >& <& 以及 2> 这类带文件描述符的重定向。
func (l *lexer) redirect() {
	// 当前单词全是数字时它是文件描述符（2>），不是参数
	if l.inWord && l.word.Len() > 0 && isDigits(l.word.String()) && !l.wordHasExpansion {
		l.word.Reset()
		l.inWord = false
	} else {
		l.endWord()
	}
	c := l.src[l.pos]
	op := string(c)
	l.pos++
	switch {
	case c == '<' && l.peek(0) == '(' || c == '>' && l.peek(0) == '(':
		l.pos++
		inner, _ := l.readBalanced('(', ')')
		l.a.unsafe("包含进程替换")
		analyze(inner, l.a, l.depth+1, true)
		l.inWord, l.wordHasExpansion = true, true
		l.word.WriteString("<(...)")
		return
	case c == '<' && l.peek(0) == '<':
		l.pos++
		op = "<<"
		if l.peek(0) == '<' {
			l.pos++
			op = "<<<"
		} else {
			l.a.unsafe("包含 heredoc")
			if l.peek(0) == '-' {
				l.pos++
			}
		}
	case c == '>' && (l.peek(0) == '>' || l.peek(0) == '|'):
		op += string(l.peek(0))
		l.pos++
	case l.peek(0) == '&':
		op += "&"
		l.pos++
	}
	l.pendingRedirect = op
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// writesFile 判断重定向是否会写入一个真实文件（/dev/null、&1、&2 之类不算）。
func (r Redirect) writesFile() bool {
	if !strings.Contains(r.Op, ">") {
		return false
	}
	switch r.Target {
	case "/dev/null", "/dev/stdout", "/dev/stderr", "-", "1", "2", "&1", "&2":
		return false
	}
	return !(strings.HasSuffix(r.Op, "&") && isDigits(r.Target))
}
