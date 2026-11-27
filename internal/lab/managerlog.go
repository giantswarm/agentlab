package lab

import "strings"

// managerLoggedUser matches the issuer and subject together. The caller
// field identifies the person only when structured identity is absent.
func managerLoggedUser(line, email, issuer, subject string) bool {
	fields := logfmtFields(line)
	_, hasIssuer := fields["user.issuer"]
	_, hasSubject := fields["user.subject"]
	if hasIssuer || hasSubject {
		return issuer != "" && subject != "" && fields["user.issuer"] == issuer && fields["user.subject"] == subject
	}
	return email != "" && fields["caller"] == email
}

// managerLogNamesUser reports whether any line of the log names the person.
func managerLogNamesUser(logs, email, issuer, subject string) bool {
	for line := range strings.SplitSeq(logs, "\n") {
		if managerLoggedUser(line, email, issuer, subject) {
			return true
		}
	}
	return false
}
