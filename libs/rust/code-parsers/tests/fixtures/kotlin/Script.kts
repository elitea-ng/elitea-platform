import java.io.File

val files = File(".").listFiles()
fun report(name: String) = println("file: $name")
files?.forEach { report(it.name) }
