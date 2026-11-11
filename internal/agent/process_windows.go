package agent

func processLister(name string) []string {
	return []string{"tasklist", "/FI", "IMAGENAME eq " + name, "/NH"}
}
