import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:marionette_flutter/marionette_flutter.dart';

// A small task list for the covey live test (live-test/README.md). The keys are
// what marionette addresses; the app carries one deliberate defect for the
// agent to find: an empty title is accepted and adds a blank task.
void main() {
  if (kDebugMode) {
    MarionetteBinding.ensureInitialized();
  } else {
    WidgetsFlutterBinding.ensureInitialized();
  }
  runApp(const TaskApp());
}

class TaskApp extends StatelessWidget {
  const TaskApp({super.key});

  @override
  Widget build(BuildContext context) {
    return const MaterialApp(title: 'Live test tasks', home: TaskPage());
  }
}

class TaskPage extends StatefulWidget {
  const TaskPage({super.key});

  @override
  State<TaskPage> createState() => _TaskPageState();
}

class _TaskPageState extends State<TaskPage> {
  final _tasks = <String>[];
  final _done = <int>{};
  final _input = TextEditingController();

  void _add() {
    // The defect: no check for an empty title.
    setState(() => _tasks.add(_input.text.trim()));
    _input.clear();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text('Tasks: ${_tasks.length - _done.length} open', key: const ValueKey('open_count'))),
      body: Column(children: [
        Padding(
          padding: const EdgeInsets.all(12),
          child: Row(children: [
            Expanded(
              child: TextField(
                key: const ValueKey('new_task'),
                controller: _input,
                decoration: const InputDecoration(labelText: 'New task'),
                onSubmitted: (_) => _add(),
              ),
            ),
            const SizedBox(width: 8),
            ElevatedButton(key: const ValueKey('add_task'), onPressed: _add, child: const Text('Add')),
          ]),
        ),
        Expanded(
          child: ListView.builder(
            itemCount: _tasks.length,
            itemBuilder: (context, i) => CheckboxListTile(
              key: ValueKey('task_$i'),
              title: Text(_tasks[i]),
              value: _done.contains(i),
              onChanged: (v) => setState(() => v == true ? _done.add(i) : _done.remove(i)),
            ),
          ),
        ),
      ]),
    );
  }
}
