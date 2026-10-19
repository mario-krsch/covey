import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:live_test_flutter/main.dart';

void main() {
  testWidgets('adding a task counts it as open', (tester) async {
    await tester.pumpWidget(const TaskApp());
    await tester.enterText(find.byKey(const ValueKey('new_task')), 'Write the recipe');
    await tester.tap(find.byKey(const ValueKey('add_task')));
    await tester.pump();
    expect(find.text('Write the recipe'), findsOneWidget);
    expect(find.text('Tasks: 1 open'), findsOneWidget);
  });

  // Known red: the baseline in covey.project.yml lists it. The app accepts an
  // empty title (the deliberate defect), so this fails without any change.
  testWidgets('an empty title is refused', (tester) async {
    await tester.pumpWidget(const TaskApp());
    await tester.tap(find.byKey(const ValueKey('add_task')));
    await tester.pump();
    expect(find.text('Tasks: 0 open'), findsOneWidget);
  });
}
