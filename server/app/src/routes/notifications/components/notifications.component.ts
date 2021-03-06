import { Component, OnInit } from '@angular/core';

import { MessageService } from '../../../services/index';

import { DiscordComponent }     from './discord.component';
import { TelegramComponent }    from './telegram.component';
import { SlackComponent }       from './slack.component';
import { SteemitChatComponent } from './steemit-chat.component';


@Component({
  moduleId: module.id,
  templateUrl: 'notifications.component.html',
  directives: [DiscordComponent, TelegramComponent, SlackComponent, SteemitChatComponent]
})
export class NotificationsComponent implements OnInit {

  constructor(private messageService: MessageService) {}

  ngOnInit() {
    this.messageService.hideMessage();
  }
}
