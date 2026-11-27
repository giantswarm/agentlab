package lab

import (
	"strconv"
	"strings"
)

// managerLoggedUser reports whether a text-format log line of a manager names
// the person: user.subject=<sub> from mcp-oauth's UserInfo, or caller=<email>
// in releases that log the caller's email.
func managerLoggedUser(line, email, subject string) bool {
	line += " "
	if subject != "" && (strings.Contains(line, " user.subject="+subject+" ") || strings.Contains(line, " user.subject="+strconv.Quote(subject)+" ")) {
		return true
	}
	return email != "" && strings.Contains(line, " caller="+email+" ")
}

// managerLogNamesUser reports whether any line of the log names the person.
func managerLogNamesUser(logs, email, subject string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if managerLoggedUser(line, email, subject) {
			return true
		}
	}
	return false
}
