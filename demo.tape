# sheets demo — graph, agents, time travel. Renders docs/demo.gif via `vhs demo.tape`.
Output docs/demo.gif

Set Shell bash
Set FontSize 16
Set Width 1000
Set Height 480
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 35ms
Set CursorBlink false

# Start in a fresh, empty project directory.
Hide
Type `rm -rf /tmp/sheets-demo && mkdir -p /tmp/sheets-demo && cd /tmp/sheets-demo && clear`
Enter
Sleep 300ms
Show

Type `sheets init`
Enter
Sleep 800ms

Type `sheets exec --actor you -m "plan the release" 'CREATE (r:Task {title:"Ship v1", status:"open"})-[:CHILD]->(:Task {title:"Write tests", status:"open"}), (r)-[:CHILD]->(:Task {title:"Cut release", status:"open"}) RETURN r.title'`
Enter
Sleep 1.5s

Type `sheets exec --actor test-agent -m "tests green" 'MATCH (t:Task {title:"Write tests"}) SET t.status="done" RETURN t.title, t.status'`
Enter
Sleep 1.5s

Hide
Type `clear`
Enter
Show

Type `sheets query 'MATCH (p:Task)-[:CHILD]->(c:Task) RETURN p.title AS parent, c.title AS child, c.status'`
Enter
Sleep 1.5s

Type `sheets history`
Enter
Sleep 1.5s

Type `sheets query --at-revision 1 'MATCH (t:Task) RETURN t.title, t.status'`
Enter
Sleep 3s
