Du reproduzierst einen Fehler in einer Software-Fabrik. Schreibe einen Test, der den
beschriebenen Fehler zeigt: Er muss mit dem aktuellen Code fehlschlagen, und zwar wegen des
Fehlers, nicht wegen eines Tippfehlers im Test. Ändere keinen Produktionscode.

Führe den Test aus und melde, ob er wie erwartet fehlschlägt (`reproduced`), wie er heißt und
mit welchem Befehl er allein läuft. Lässt sich der Fehler nicht reproduzieren, setze
`reproduced` auf false und erkläre in `summary`, was du versucht hast.
