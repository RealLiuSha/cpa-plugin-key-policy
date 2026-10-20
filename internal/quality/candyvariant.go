package quality

import (
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
)

// The inspection asks variants of the candy question: the counts, flavors,
// shapes and column order change, the reasoning does not. The published
// question can be recognized and answered from memory, a variant has to be
// solved. Shapes can be told apart by touch, so whoever draws chooses how
// many of each shape to take; candyAnswer finds the fewest that guarantee a
// pair whatever the bag holds.

var (
	candyItems  = []string{"糖果", "软糖", "水果糖"}
	candyKinds  = []string{"苹果", "桃子", "西瓜", "葡萄", "草莓", "橙子", "芒果", "柠檬", "菠萝", "蓝莓"}
	candyShapes = [][2]string{{"圆形", "五角星形"}, {"圆形", "方形"}, {"心形", "星形"}, {"三角形", "圆形"}, {"方形", "心形"}}
)

// Counts of a variant lie in this range, as in the published question.
const (
	candyMinCount = 4
	candyMaxCount = 12
)

// CandyVariant is one candy question. Kinds[0] and Kinds[1] are the flavors
// asked for and Kinds[2] the third; Counts[shape][kind] is the table, whose
// columns are shown in Order; Answer is the fewest candies that guarantee the
// pair.
type CandyVariant struct {
	Item   string    `json:"item"`
	Kinds  [3]string `json:"kinds"`
	Shapes [2]string `json:"shapes"`
	Counts [2][3]int `json:"counts"`
	Order  [3]int    `json:"order"`
	Answer int       `json:"answer"`
}

// candyAnswer is the fewest candies, r of the first shape and s of the
// second, that guarantee the first shape of kind 0 with the second of kind
// 1, or the first of kind 1 with the second of kind 0. A draw avoids both
// pairs only by missing one kind in each: no kind 0 or 1 of the first shape
// (r within its third kind), none of kind 0 at all, none of kind 1 at all,
// or no kind 0 or 1 of the second shape. The answer leaves each of these
// impossible.
func candyAnswer(c [2][3]int) int {
	first, second := c[0][0]+c[0][1]+c[0][2], c[1][0]+c[1][1]+c[1][2]
	best := first + second
	for r := 0; r <= first; r++ {
		for s := 0; s <= second; s++ {
			if r > c[0][2] && s > c[1][2] &&
				(r > c[0][1]+c[0][2] || s > c[1][1]+c[1][2]) &&
				(r > c[0][0]+c[0][2] || s > c[1][0]+c[1][2]) {
				best = min(best, r+s)
			}
		}
	}
	return best
}

// candyTraps are the answers of the usual mistakes: drawing blind (the
// largest draw that misses both pairs, plus one) and securing one pair
// outright. A variant whose answer is one of them, or not well below them,
// does not tell reasoning apart from them.
func candyTraps(c [2][3]int) []int {
	first, second := c[0][0]+c[0][1]+c[0][2], c[1][0]+c[1][1]+c[1][2]
	blind := max(c[0][2]+second, c[0][1]+c[0][2]+c[1][1]+c[1][2], c[0][0]+c[0][2]+c[1][0]+c[1][2], first+c[1][2]) + 1
	pair01 := (c[0][1] + c[0][2] + 1) + (c[1][0] + c[1][2] + 1)
	pair10 := (c[0][0] + c[0][2] + 1) + (c[1][1] + c[1][2] + 1)
	return []int{blind, pair01, pair10}
}

// NewCandyVariant draws a variant whose answer is at least 4 below every
// trap.
func NewCandyVariant(rng *rand.Rand) CandyVariant {
	for {
		v := CandyVariant{Item: candyItems[rng.Intn(len(candyItems))], Shapes: candyShapes[rng.Intn(len(candyShapes))]}
		kinds := rng.Perm(len(candyKinds))
		for i := range v.Kinds {
			v.Kinds[i] = candyKinds[kinds[i]]
		}
		if rng.Intn(2) == 1 {
			v.Shapes[0], v.Shapes[1] = v.Shapes[1], v.Shapes[0]
		}
		copy(v.Order[:], rng.Perm(3))
		for shape := range v.Counts {
			for kind := range v.Counts[shape] {
				v.Counts[shape][kind] = candyMinCount + rng.Intn(candyMaxCount-candyMinCount+1)
			}
		}
		v.Answer = candyAnswer(v.Counts)
		if trap := candyTraps(v.Counts); v.Answer+4 <= min(trap[0], trap[1], trap[2]) {
			return v
		}
	}
}

// Prompt is the question as the inspection asks it, worded like the
// published one, with a last line that makes the answer easy to find.
func (v CandyVariant) Prompt() string {
	item, k, s := v.Item, v.Kinds, v.Shapes
	var b strings.Builder
	fmt.Fprintf(&b, "不使用任何外部工具回答以下问题：\n\n在一个黑色的袋子里放有三种口味的%s，每种%s有两种不同的形状（%s和%s，不同的形状靠手感可以分辨）。"+
		"现已知不同口味的%s和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的%s数目，那么，最少取出多少个%s才能保证手中同时拥有不同形状的%s味和%s味的%s？"+
		"（同时手中有%s%s味匹配%s%s味%s，或者有%s%s味匹配%s%s味%s都满足要求）\n\n",
		item, item, s[0], s[1], item, item, item, k[0], k[1], item, s[0], k[0], s[1], k[1], item, s[0], k[1], s[1], k[0], item)
	header := "        "
	for _, kind := range v.Order {
		header += k[kind] + "味  "
	}
	b.WriteString(strings.TrimRight(header, " "))
	for shape, name := range s {
		row := name + strings.Repeat(" ", max(1, 9-2*len([]rune(name))))
		for _, kind := range v.Order {
			row += fmt.Sprintf("%-7d", v.Counts[shape][kind])
		}
		b.WriteString("\n" + strings.TrimRight(row, " "))
	}
	b.WriteString("\n\n最后单独写一行「答案：」加上数字。\n")
	return b.String()
}

var (
	// candyAnswerLine is the line the question asks for, markdown allowed:
	// "答案：21", "**答案**: 21", or the number on the next line.
	candyAnswerLine = regexp.MustCompile(`(?m)^[\s*#>]*答案[\s*]*[:：][\s*]*(\d+)`)
	// candyAnswerWord is a looser mention, such as "答案是 21".
	candyAnswerWord = regexp.MustCompile(`答案[^0-9\n]{0,12}?(\d+)`)
	candyNumber     = regexp.MustCompile(`\d+`)
)

// candyVariantCorrect judges an answer to a variant by the number of its last
// answer line, else of its last mention of 答案, else its last number.
// Full-width digits count as digits.
func candyVariantCorrect(answer string, expected int) bool {
	answer = strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return '0' + r - '０'
		}
		return r
	}, answer)
	number := ""
	if matches := candyAnswerLine.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		number = matches[len(matches)-1][1]
	} else if matches := candyAnswerWord.FindAllStringSubmatch(answer, -1); len(matches) > 0 {
		number = matches[len(matches)-1][1]
	} else if numbers := candyNumber.FindAllString(answer, -1); len(numbers) > 0 {
		number = numbers[len(numbers)-1]
	}
	n, err := strconv.Atoi(number)
	return err == nil && n == expected
}
